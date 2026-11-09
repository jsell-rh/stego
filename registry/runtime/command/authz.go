package command

import (
	"context"
	"errors"
	"fmt"

	"stego.dev/runtime/authz"
)

// ErrNoResource is the error of an evaluation with no loaded value of the handle: every decision of a fill is on a
// row that the handle read, at the floor that its token and the revocation floor give (ST-SUP-RP10-LC04).
var ErrNoResource = errors.New("command: an evaluation needs a loaded value of this handle with a resource type")

// Authz is the handle's authorization part (ST-SUP-RP10-LC04): the authorization runtime's handle bound to the same
// transaction, tenant and principal. Each evaluation decides with the principal as subject at the consistency floor
// of ST-SUP-RP06-LC09, on a row that the handle loaded; Relate and Unrelate only enqueue tuples for the relay in the
// transaction's outbox, for the relations that the command or the worker declares. It offers no other decision path
// and exposes neither the provider nor the relationships port.
type Authz struct {
	st *state
}

// Evaluate decides action on the resource of target, a loaded value of this handle, with the handle's principal as
// subject, at least as fresh as the revocation floor, or as the row's token while the floor is zero. A deny is
// false with a nil error.
func (a *Authz) Evaluate(ctx context.Context, action string, target Ref, c authz.Context) (bool, error) {
	s := a.st
	if err := s.enter(); err != nil {
		return false, err
	}
	defer s.leave()
	if target == nil {
		return false, ErrNoResource
	}
	r := refOf(target)
	if r.st != s || r.resourceType == "" {
		return false, ErrNoResource
	}
	return s.evaluate(ctx, s.az, action, r.resourceType, r.id, r.token, c)
}

// Relate enqueues a touch of tuple for the relay in the transaction's outbox and returns the transaction's
// revocation; it refuses a relation that the command or the worker does not declare (ST-SUP-RP06-LC10).
func (a *Authz) Relate(ctx context.Context, tuple authz.Tuple) (authz.Revocation, error) {
	return a.write(ctx, tuple, (*authz.Handle).Relate)
}

// Unrelate enqueues a delete of tuple for the relay, as Relate does.
func (a *Authz) Unrelate(ctx context.Context, tuple authz.Tuple) (authz.Revocation, error) {
	return a.write(ctx, tuple, (*authz.Handle).Unrelate)
}

func (a *Authz) write(ctx context.Context, tuple authz.Tuple,
	op func(*authz.Handle, context.Context, authz.Tuple) (authz.Revocation, error)) (authz.Revocation, error) {
	s := a.st
	if err := s.enter(); err != nil {
		return authz.Revocation{}, err
	}
	defer s.leave()
	if err := s.setLockTimeout(ctx); err != nil {
		return authz.Revocation{}, s.fail(ctx, "relate", err)
	}
	revocation, err := op(s.az, ctx, tuple)
	if err != nil {
		return authz.Revocation{}, s.decisionError(ctx, "relate", err)
	}
	return revocation, nil
}

// evaluate decides one row through handle, an authorization handle bound to this transaction.
func (s *state) evaluate(ctx context.Context, handle *authz.Handle, action, resourceType, id, token string,
	c authz.Context) (bool, error) {
	rowToken, err := authz.ParseToken(token)
	if err != nil {
		return false, s.breakWith(internalf("a row of %s holds an authorization token that cannot be read", resourceType))
	}
	result, err := handle.Evaluate(ctx, action, authz.Resource{Type: resourceType, ID: id}, rowToken, c)
	if err != nil {
		return false, s.decisionError(ctx, "decide", err)
	}
	return result.Allowed, nil
}

// decisionError maps an error of the authorization handle: a database error or a cancel is a statement failure of
// the transaction, and any other error, such as a provider error, fails the call only.
func (s *state) decisionError(ctx context.Context, step string, err error) error {
	if sqlState(err) != "" || ctx.Err() != nil {
		return s.fail(ctx, step, err)
	}
	return fmt.Errorf("command: %s: %w", step, err)
}

// decideRead decides the read of one row of an entity with a resource type. A command decides the entity's read
// permission (ST-SUP-RP10-LC13, ST-SUP-RP11-LC06) and answers a deny as not found; a worker decides its own
// permission, and a consumer also the staging principal with the stager's permission, and answers a deny with the
// refused error, which is also not found (ST-SUP-RP10-LC17, ST-SUP-RP10-LC23).
func (s *state) decideRead(ctx context.Context, resourceType, permission, entity string, meta Meta) error {
	if resourceType == "" {
		return nil
	}
	if s.mode == modeCommand {
		ok, err := s.evaluate(ctx, s.az, permission, resourceType, meta.ID, meta.Token, nil)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s", ErrNotFound, entity)
		}
		return nil
	}
	ok, staging, err := s.decideBoth(ctx, resourceType, meta.ID, meta.Token)
	if err != nil {
		return err
	}
	if !ok {
		return &deniedError{entity: entity, read: true, staging: staging}
	}
	return nil
}

// decideWrite decides a worker's write on one resource, before the write (ST-SUP-RP10-LC17). A command makes no
// decision per write: its declared permission, decided before the fill, covers its declared writes
// (ST-SUP-RP10-LC05).
func (s *state) decideWrite(ctx context.Context, resourceType, entity, id, token string) error {
	if s.mode == modeCommand || resourceType == "" {
		return nil
	}
	ok, staging, err := s.decideBoth(ctx, resourceType, id, token)
	if err != nil {
		return err
	}
	if !ok {
		return &deniedError{entity: entity, staging: staging}
	}
	return nil
}

// decideBoth decides a worker's permission with its service account as subject and, in a consumer, only after an
// allow, the stager's permission with the staging principal as subject. staging is true for a deny of the staging
// principal.
func (s *state) decideBoth(ctx context.Context, resourceType, id, token string) (ok, staging bool, err error) {
	ok, err = s.evaluate(ctx, s.az, s.permission, resourceType, id, token, nil)
	if err != nil || !ok {
		return false, false, err
	}
	if s.mode != modeConsumer {
		return true, false, nil
	}
	ok, err = s.evaluate(ctx, s.staging, s.stagingPermission, resourceType, id, token, nil)
	return ok, !ok && err == nil, err
}

// decideList filters the rows of a list by batch evaluation (ST-SUP-RP11-LC07): a command on the entity's list
// permission, or its read permission when it declares none; a worker on its own permission, and a consumer also as
// the staging principal. It returns one answer for each row.
func (s *state) decideList(ctx context.Context, resourceType, permission string, metas []Meta) ([]bool, error) {
	allowed := make([]bool, len(metas))
	if resourceType == "" {
		for i := range allowed {
			allowed[i] = true
		}
		return allowed, nil
	}
	if s.mode == modeCommand {
		return s.batch(ctx, s.az, permission, resourceType, metas, allowed, nil)
	}
	if _, err := s.batch(ctx, s.az, s.permission, resourceType, metas, allowed, nil); err != nil {
		return nil, err
	}
	if s.mode != modeConsumer {
		return allowed, nil
	}
	return s.batch(ctx, s.staging, s.stagingPermission, resourceType, metas, allowed, allowed)
}

// batch decides permission on the rows of metas in one batch evaluation and writes the answers to allowed. When only
// is not nil, it decides only the rows that only allows, and leaves the others denied.
func (s *state) batch(ctx context.Context, handle *authz.Handle, permission, resourceType string, metas []Meta,
	allowed, only []bool) ([]bool, error) {
	var rows []authz.Row
	var index []int
	for i, meta := range metas {
		if only != nil && !only[i] {
			continue
		}
		token, err := authz.ParseToken(meta.Token)
		if err != nil {
			return nil, s.breakWith(internalf("a row of %s holds an authorization token that cannot be read", resourceType))
		}
		rows = append(rows, authz.Row{Resource: authz.Resource{Type: resourceType, ID: meta.ID}, Token: token})
		index = append(index, i)
	}
	if len(rows) == 0 {
		return allowed, nil
	}
	results, err := handle.EvaluateBatch(ctx, permission, rows, nil)
	if err != nil {
		return nil, s.decisionError(ctx, "decide the list", err)
	}
	if len(results) != len(rows) {
		return nil, fmt.Errorf("command: the decision port answered %d of %d rows", len(results), len(rows))
	}
	for j, i := range index {
		allowed[i] = results[j].Allowed
	}
	return allowed, nil
}

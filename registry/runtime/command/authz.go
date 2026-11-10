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
// revocation; it refuses a relation that the command or the worker does not declare (ST-SUP-RP06-LC10). on is the
// loaded value of this handle whose row is the tuple's resource: Relate refuses a tuple whose resource is not that
// row, and decides on it first, so a fill cannot change the relationships of a row it may not act on. A command
// decides its declared permission on the row, and a command that declares none may relate nothing; a worker decides
// its own permission, and a consumer also the stager's (ST-SUP-RP10-LC05, ST-SUP-RP10-LC17). A deny is not found.
func (a *Authz) Relate(ctx context.Context, on Ref, tuple authz.Tuple) (authz.Revocation, error) {
	return a.write(ctx, on, tuple, (*authz.Handle).Relate)
}

// Unrelate enqueues a delete of tuple for the relay, on the row of on, as Relate does.
func (a *Authz) Unrelate(ctx context.Context, on Ref, tuple authz.Tuple) (authz.Revocation, error) {
	return a.write(ctx, on, tuple, (*authz.Handle).Unrelate)
}

func (a *Authz) write(ctx context.Context, on Ref, tuple authz.Tuple,
	op func(*authz.Handle, context.Context, authz.Tuple) (authz.Revocation, error)) (authz.Revocation, error) {
	s := a.st
	if err := s.enter(); err != nil {
		return authz.Revocation{}, err
	}
	defer s.leave()
	if on == nil {
		return authz.Revocation{}, ErrNoResource
	}
	r := refOf(on)
	if r.st != s || r.resourceType == "" {
		return authz.Revocation{}, ErrNoResource
	}
	resource, err := authz.NewResource(s.tenant, r.resourceType, r.id)
	if err != nil || tuple.Resource != resource {
		return authz.Revocation{}, s.breakWith(internalf("a tuple of %s names another resource than the loaded value it is for", r.entity))
	}
	if s.mode == modeCommand {
		if s.commandPermission == "" {
			return authz.Revocation{}, errors.New("command: a command that declares no permission relates nothing")
		}
		if err := s.decideCommandOn(ctx, r.resourceType, r.entity, r.id, r.token); err != nil {
			return authz.Revocation{}, err
		}
	} else if err := s.decideWrite(ctx, r.resourceType, r.entity, r.id, r.token); err != nil {
		return authz.Revocation{}, err
	}
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
	// The resource ID at the provider is qualified by the tenant (ST-SUP-RP06-LC22): the key is unique only in it.
	resource, err := authz.NewResource(s.tenant, resourceType, id)
	if err != nil {
		return false, fmt.Errorf("%w: the key of a resource of %s", ErrNotFound, resourceType)
	}
	result, err := handle.Evaluate(ctx, action, resource, rowToken, c)
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

// decideWrite decides a write on one resource, before the write. A worker decides its own permission
// (ST-SUP-RP10-LC17); a command decides its declared permission on the row, so a fill cannot write a row other than
// the decided resource without the same permission on it, and a deny answers as not found (ST-SUP-RP10-LC05).
func (s *state) decideWrite(ctx context.Context, resourceType, entity, id, token string) error {
	if s.mode == modeCommand {
		return s.decideCommandOn(ctx, resourceType, entity, id, token)
	}
	if resourceType == "" {
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

// decideCommandOn decides the command's declared permission on one row of an entity with a resource type, at the
// floor and the row's token, for a locked read or a write of a command (ST-SUP-RP10-LC05). A deny is ErrNotFound. A
// command with no permission, a worker and an entity with no resource type decide nothing here.
func (s *state) decideCommandOn(ctx context.Context, resourceType, entity, id, token string) error {
	if s.mode != modeCommand || s.commandPermission == "" || resourceType == "" {
		return nil
	}
	ok, err := s.evaluate(ctx, s.az, s.commandPermission, resourceType, id, token, nil)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, entity)
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
		resource, err := authz.NewResource(s.tenant, resourceType, meta.ID)
		if err != nil {
			return nil, s.breakWith(internalf("a row of %s has a key that is not a resource key", resourceType))
		}
		rows = append(rows, authz.Row{Resource: resource, Token: token})
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

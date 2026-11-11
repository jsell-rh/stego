package command

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
)

// Destination is one outbox destination that a command or a worker declares it stages effects for: its name, whether
// its messages must land and whether it is idempotent, as the destination is registered on the outbox worker, and
// whether a worker consumes it, so each message stores the staging principal and stager (ST-SUP-RP10-LC16). Effects
// are the effect names that the declaration states for the destination: a command's Stage refuses every other name
// (ST-SUP-RP10-LC07), and a worker that states none may stage any name.
type Destination struct {
	Name       string
	MustLand   bool
	Idempotent bool
	Consumed   bool
	Effects    []string
}

// reservedDestinations are the destinations that only the authorization handle and the audit runtime write to
// (ST-SUP-RP10-LC07).
var reservedDestinations = map[string]bool{authz.DestinationName: true, audit.SinkDestination: true}

// checkDestinations checks the declared destinations of a command or a worker and maps them by name.
func checkDestinations(owner string, destinations []Destination) (map[string]Destination, error) {
	byName := make(map[string]Destination, len(destinations))
	for _, d := range destinations {
		if reservedDestinations[d.Name] {
			return nil, fmt.Errorf("command: %s declares the destination %s, which only the runtime writes to", owner, d.Name)
		}
		if d.Name == "" {
			return nil, fmt.Errorf("command: %s declares a destination with no name", owner)
		}
		if _, ok := byName[d.Name]; ok {
			return nil, fmt.Errorf("command: %s declares the destination %s twice", owner, d.Name)
		}
		for i, effect := range d.Effects {
			if effect == "" || slices.Contains(d.Effects[:i], effect) {
				return nil, fmt.Errorf("command: %s declares an empty or a repeated effect name for the destination %s", owner, d.Name)
			}
		}
		d.Effects = slices.Clone(d.Effects)
		byName[d.Name] = d
	}
	return byName, nil
}

// Effects is the handle's effect stager (ST-SUP-RP10-LC07): each effect becomes an outbox message of the bound
// transaction, enqueued with the other messages of the attempt after the fill returns, so nothing is delivered
// before the commit. It holds no broker, provider or network client.
type Effects struct {
	st *state
}

// Stage stages the effect named effect for the declared destination, for the loaded value entity of this handle,
// with payload, a JSON value (ST-SUP-RP10-LC08). The message's outgoing idempotency key is Derive over the tenant,
// the principal, the inbound key, the destination and the effect name, where the inbound key is the run's inbound
// key and then the entity's ID, each prefixed by its length. It refuses the destinations authz and audit-sink, an
// undeclared destination, in a command an effect name that the command does not declare for the destination, which
// breaks the call so it rolls back, a value of another handle, and a second effect of one name for one entity in one call,
// and then stages nothing. In a command it decides the command's permission on an entity with a resource type, as
// a write does, and refuses an entity with none unless the value was locked or written.
func (f *Effects) Stage(ctx context.Context, destination, effect string, entity Ref, payload json.RawMessage, parent ...Ref) error {
	s := f.st
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	if reservedDestinations[destination] {
		return fmt.Errorf("command: no effect goes to the destination %s: it is written only by the runtime", destination)
	}
	d, ok := s.destinations[destination]
	if !ok {
		return fmt.Errorf("command: %s does not declare the destination %s", s.action, destination)
	}
	// A command stages only the effect names that its declaration states for the destination (ST-SUP-RP10-LC07).
	if (s.mode == modeCommand || len(d.Effects) > 0) && !slices.Contains(d.Effects, effect) {
		return s.breakWith(internalf("%s does not declare the effect %s for the destination %s", s.action, effect, destination))
	}
	if entity == nil {
		return errors.New("command: an effect needs the loaded value it is for")
	}
	r := refOf(entity)
	if r.st != s {
		return s.breakWith(internalf("an effect of %s names a loaded value of another transaction", s.action))
	}
	// An effect is for a row the command or the worker may act on (ST-SUP-RP10-LC05): a command decides its
	// permission on an entity with a resource type; a child entity, which has a Parent and no resource type, takes
	// the decision of its parent, whose loaded value parent must give, as a write of it does; and any other entity in
	// a command needs a value that the handle locked or wrote.
	child, _ := r.entityRef.(interface {
		parentsOf(Ref) ([]reference, bool)
	})
	var refs []reference
	isChild := false
	if child != nil {
		refs, isChild = child.parentsOf(entity)
	}
	parent = nonNil(parent)
	switch {
	case isChild:
		used := make([]bool, len(parent))
		rows, err := s.matchParents("effect", r.entity, refs, parent, used)
		if err != nil {
			return err
		}
		if err := s.allUsed("effect", r.entity, used); err != nil {
			return err
		}
		for _, p := range rows {
			if err := s.decideWrite(ctx, p.resourceType, p.entity, p.id, p.token); err != nil {
				return err
			}
		}
	case len(parent) > 0:
		return s.breakWith(internalf("an effect of %s for %s takes no parent: it is not a child entity", s.action, r.entity))
	case s.mode != modeCommand:
	case r.resourceType != "":
		if err := s.decideCommandOn(ctx, r.resourceType, r.entity, r.id, r.token); err != nil {
			return err
		}
	default:
		if _, written := s.written[rowKey{r.entity, r.id}]; r.lock == lockNone && !written {
			return fmt.Errorf("command: an effect of %s for %s needs a value that the handle locked or wrote", s.action, r.entity)
		}
	}
	// One effect of one name for one entity in one call; and, as two entities of two types can share an ID, never
	// two messages with one key.
	unique := lengthPrefixed("effect", effect, r.resourceKey())
	principal := s.principal.Subject().String()
	key := idempotency.Derive(s.tenant.ID(), principal, s.inbound(r.id), destination, effect)
	if s.stagedK[unique] || s.stagedK[lengthPrefixed("key", key)] {
		return fmt.Errorf("command: the effect %s is staged already for this %s in this call", effect, r.entity)
	}
	if len(s.staged) >= outbox.MaxBatchSize {
		return fmt.Errorf("command: a call stages at most %d effects", outbox.MaxBatchSize)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	message := outbox.Message{
		ID: id, Destination: destination, ResourceKey: r.resourceKey(), Kind: effect,
		// A copy, so a fill that changes its payload after the call changes nothing that is staged.
		Payload:     bytes.Clone(payload),
		OutgoingKey: key, MustLand: d.MustLand, Idempotent: d.Idempotent,
	}
	if d.Consumed {
		message.Principal, message.Origin = principal, s.action
	}
	if err := outbox.ValidateMessage(message); err != nil {
		return err
	}
	s.staged = append(s.staged, message)
	s.stagedK[unique] = true
	s.stagedK[lengthPrefixed("key", key)] = true
	return nil
}

// enqueue writes the staged messages of the attempt into the bound transaction's outbox in one batch, after the fill
// returned and before the audit record (ST-SUP-RP10-LC07). A conflict of the enqueue is a conflict of the call
// (ST-SUP-RP10-LC25).
func (s *state) enqueue(ctx context.Context) error {
	if len(s.staged) == 0 {
		return nil
	}
	if err := s.setLockTimeout(ctx); err != nil {
		return s.fail(ctx, "outbox enqueue", err)
	}
	s.rt.stepped("enqueue", s.tx)
	if err := s.rt.cfg.Outbox.Enqueue(ctx, s.tx, s.staged...); err != nil {
		return s.fail(ctx, "outbox enqueue", err)
	}
	return nil
}

// lengthPrefixed joins parts, each prefixed by its length in bytes as a 4-byte big-endian unsigned integer.
func lengthPrefixed(parts ...string) string {
	var b []byte
	for _, part := range parts {
		b = binary.BigEndian.AppendUint32(b, uint32(len(part)))
		b = append(b, part...)
	}
	return string(b)
}

// commandInbound is the inbound key of an effect of a command: the command's inbound key and then the entity's ID.
func commandInbound(key string) func(id string) string {
	return func(id string) string { return lengthPrefixed(key, id) }
}

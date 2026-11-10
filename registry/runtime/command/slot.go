package command

import (
	"errors"

	"github.com/jsell-rh/stego/registry/runtime/internal/loaded"
)

// slotKey is the key of the generated slot code of one service (ST-SUP-RP10-LC32). Its type is not exported, so no
// code outside this package can name it, write a literal of it or pass nil for it: only a value that SlotKey gave
// works. The generated slot code gets it from the runtime that the service's generated wiring holds, and a fill
// gets neither the runtime nor the key.
type slotKey struct {
	rt *Runtime
}

// slotSeal is what only New sets on a runtime: the entities that the service registered.
type slotSeal struct {
	entities map[Declared]bool
}

// SlotKey returns the key with which the generated slot code of the service makes the loaded value of a request
// entity (FromRequest). Only the generated code that holds the runtime calls it; the handle that a fill gets gives
// no way to reach the runtime. A nil runtime gives a key that FromRequest refuses.
func (rt *Runtime) SlotKey() slotKey { return slotKey{rt: rt} }

// ErrNoSlotKey is the error of FromRequest with a key that no runtime gave.
var ErrNoSlotKey = errors.New("command: a loaded value of a request needs the slot key of the runtime")

// ErrNotRegistered is the error of FromRequest for an entity that the runtime's Config.Entities does not hold, such as
// one that a fill made with NewEntity.
var ErrNotRegistered = errors.New("command: the entity is not an entity of this runtime")

// FromRequest makes the loaded value of the entity e from value, the request entity, after the generated slot code
// has validated the request (ST-SUP-RP10-LC32, ST-SUP-RC34-OC08). The generated code gives it to the fill beside the
// probe, so a probe method of the slot binding reads its entity fields from a value that the fill did not build.
//
// The value is a deep copy of value, made with the entity's Clone, so a later change to the request entity does not
// change it. It names no row and no handle state: every handle refuses it for a write, an effect or a decision,
// because none of them read or wrote it. Its type is the generated loaded type, such as LoadedReservation, which no
// composite literal and no conversion can make; and because the key's type is not exported, a fill cannot call
// FromRequest in a way that compiles.
func FromRequest[T any](key slotKey, e *Entity[T], value T) (Loaded[T], error) {
	if key.rt == nil || key.rt.slots == nil {
		return nil, ErrNoSlotKey
	}
	if e == nil || !key.rt.slots.entities[e] {
		return nil, ErrNotRegistered
	}
	return loaded.New(e.spec.Clone(value), e.spec.Clone, loaded.Ref{Entity: e, EntityName: e.spec.Name}), nil
}

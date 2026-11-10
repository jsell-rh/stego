// Package loaded holds the loaded value of the runtime library: the value of one row that a read or a write
// through a command handle (ST-SUP-RP10-LC15) or a query handle (ST-SUP-RP11) gave. The two handles share the one
// type, so a generated probe method takes the loaded type of an entity from either handle (ST-SUP-RC34-OC01).
//
// Value has no exported field, and only New makes one. Only a package of github.com/jsell-rh/stego/registry/runtime can import this package,
// so a fill cannot call New or Of: the generated loaded type is a pointer to Value, such as
// command.Loaded[Reservation], which no composite literal and no conversion from another type can make.
package loaded

// Ref is what a handle keeps of a loaded value besides its field values: the handle state and the entity that made
// it, which the handle compares by identity, and the row's ID, resource type, version, authorization token text
// and the lock that its read took.
type Ref struct {
	Owner        any
	Entity       any
	EntityName   string
	ResourceType string
	ID           string
	Version      int64
	Token        string
	Lock         uint8
}

// Value is the loaded value of one row of the entity type T.
type Value[T any] struct {
	value T
	clone func(T) T
	ref   Ref
}

// New makes the loaded value of value with ref, for the handles of the runtime module only. clone is the generated
// deep copy of the entity type, which Value uses; nil is a plain copy, for a type with no slice, map or pointer.
func New[T any](value T, clone func(T) T, ref Ref) *Value[T] {
	return &Value[T]{value: value, clone: clone, ref: ref}
}

// Value returns a deep copy of the stored field values, so a change to a slice or a map that the fill got never
// changes what the handle later sends, writes or decides on (ST-SUP-RP10-LC15, ST-SUP-RP11-LC12). A nil value gives
// the zero value.
func (v *Value[T]) Value() T {
	if v == nil {
		var zero T
		return zero
	}
	if v.clone != nil {
		return v.clone(v.value)
	}
	return v.value
}

// ID returns the ID of the row; a nil value gives the empty ID.
func (v *Value[T]) ID() string {
	if v == nil {
		return ""
	}
	return v.ref.ID
}

func (v *Value[T]) loadedRef() (Ref, bool) {
	if v == nil {
		return Ref{}, false
	}
	return v.ref, true
}

// Any is a loaded value of any entity type. Only *Value implements it.
type Any interface {
	loadedRef() (Ref, bool)
}

// Of returns the Ref of a, and false for a nil interface or a nil value.
func Of(a Any) (Ref, bool) {
	if a == nil {
		return Ref{}, false
	}
	return a.loadedRef()
}

// Fields returns the field values and the Ref of v, and false for a nil value.
func Fields[T any](v *Value[T]) (T, Ref, bool) {
	if v == nil {
		var zero T
		return zero, Ref{}, false
	}
	return v.value, v.ref, true
}

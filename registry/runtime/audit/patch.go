package audit

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Op is one RFC 6902 operation. Value is used by add, replace and test.
type Op struct {
	Op    string
	Path  string
	Value any
}

// Patch is an RFC 6902 JSON patch.
type Patch []Op

func (p Patch) tree() []any {
	out := make([]any, 0, len(p))
	for _, o := range p {
		m := map[string]any{"op": o.Op, "path": o.Path}
		switch o.Op {
		case "add", "replace", "test":
			m["value"] = o.Value
		}
		out = append(out, m)
	}
	return out
}

// Diff returns the RFC 6902 patch from the redacted before to the redacted after (ST-SUP-RP05-LC05). A nil state
// is JSON null. A pii field at any depth whose value changed gets a replace with RedactionMarker, so the patch
// shows the change and not the value; a secret field is in no operation.
func Diff(before, after map[string]any, f Fields) (Patch, error) {
	if err := checkStateDepth(before); err != nil {
		return nil, err
	}
	if err := checkStateDepth(after); err != nil {
		return nil, err
	}
	oldPII, newPII := map[string][]byte{}, map[string][]byte{}
	var a, b any
	if before != nil {
		r, err := redactAt("", "", before, f, oldPII)
		if err != nil {
			return nil, err
		}
		a = r
	}
	if after != nil {
		r, err := redactAt("", "", after, f, newPII)
		if err != nil {
			return nil, err
		}
		b = r
	}
	var p Patch
	diffStates(a, b, &p)
	paths := make(map[string]bool, len(p))
	for _, o := range p {
		paths[o.Path] = true
	}
	var changed []string
	for ptr, ov := range oldPII {
		if nv, ok := newPII[ptr]; ok && !bytes.Equal(ov, nv) && !covered(paths, ptr) {
			changed = append(changed, ptr)
		}
	}
	sort.Strings(changed)
	for _, ptr := range changed {
		p = append(p, Op{Op: "replace", Path: ptr, Value: RedactionMarker})
	}
	return p, nil
}

// covered reports an operation on ptr or on a member above it. paths holds the path of each operation, so the
// check reads one entry for each segment of ptr, which is at most MaxDepth+1 long.
func covered(paths map[string]bool, ptr string) bool {
	if paths[""] || paths[ptr] {
		return true
	}
	for i := 1; i < len(ptr); i++ {
		if ptr[i] == '/' && paths[ptr[:i]] {
			return true
		}
	}
	return false
}

// opOverhead is the bytes that one operation adds to the encoded patch beside its path and value.
const opOverhead = 32

// fieldBudget is the most bytes that the operations of one field of a state may take, in paths and fixed parts:
// twice the size of the field's two values and 4 KiB. Beyond it the field gets one replace of its whole value, so
// the patch stays linear in the size of the states for any shape (ST-SUP-RP05-LC05).
func fieldBudget(a, b any) int {
	return 2*(len(encodeTree(a))+len(encodeTree(b))) + 4096
}

// diffStates appends the operations from the state a to the state b, one field at a time. The operations of a
// field that would pass its budget are replaced by one replace of the field.
func diffStates(a, b any, p *Patch) {
	am, aIsMap := a.(map[string]any)
	bm, bIsMap := b.(map[string]any)
	if !aIsMap || !bIsMap {
		diffValue("", a, b, p)
		return
	}
	keys := make([]string, 0, len(am)+len(bm))
	for k := range am {
		keys = append(keys, k)
	}
	for k := range bm {
		if _, ok := am[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		segment := escapePointer(k)
		av, inA := am[k]
		bv, inB := bm[k]
		switch {
		case inA && !inB:
			*p = append(*p, Op{Op: "remove", Path: "/" + segment})
		case !inA && inB:
			*p = append(*p, Op{Op: "add", Path: "/" + segment, Value: bv})
		default:
			var field Patch
			d := differ{segments: []string{segment}, p: &field, budget: fieldBudget(av, bv)}
			d.diff(av, bv)
			if d.over {
				field = Patch{{Op: "replace", Path: "/" + segment, Value: bv}}
			}
			*p = append(*p, field...)
		}
	}
}

// diffValue appends the operations from a to b at path. It recurses into the members of two objects and of two
// arrays, and compares only two values that are not both objects or both arrays, so its work is linear in the
// size of the two values. The path of a member is made only for an operation.
func diffValue(path string, a, b any, p *Patch) {
	d := differ{p: p}
	if path != "" {
		d.segments = []string{strings.TrimPrefix(path, "/")}
	}
	d.diff(a, b)
}

// differ holds the escaped segments of the JSON pointer of the value it compares.
type differ struct {
	segments []string
	p        *Patch
	// budget, when it is not 0, is the most bytes that the paths and fixed parts of the operations may take; over
	// tells that they would pass it, and the compare then stops.
	budget, used int
	over         bool
}

// spend counts the bytes of one operation at the current path and tells whether it fits the budget.
func (d *differ) spend(segment string) bool {
	if d.budget == 0 {
		return true
	}
	n := opOverhead + len(segment)
	for _, s := range d.segments {
		n += len(s) + 1
	}
	d.used += n
	if d.used > d.budget {
		d.over = true
		return false
	}
	return true
}

func (d *differ) path() string {
	if len(d.segments) == 0 {
		return ""
	}
	return "/" + strings.Join(d.segments, "/")
}

func (d *differ) op(op string, segment string, value any, withValue bool) {
	if d.over || !d.spend(segment) {
		return
	}
	d.segments = append(d.segments, segment)
	o := Op{Op: op, Path: d.path()}
	if withValue {
		o.Value = value
	}
	*d.p = append(*d.p, o)
	d.segments = d.segments[:len(d.segments)-1]
}

func (d *differ) child(segment string, a, b any) {
	if d.over {
		return
	}
	d.segments = append(d.segments, segment)
	d.diff(a, b)
	d.segments = d.segments[:len(d.segments)-1]
}

func (d *differ) diff(a, b any) {
	am, aIsMap := a.(map[string]any)
	bm, bIsMap := b.(map[string]any)
	if aIsMap && bIsMap {
		keys := make([]string, 0, len(am)+len(bm))
		for k := range am {
			keys = append(keys, k)
		}
		for k := range bm {
			if _, ok := am[k]; !ok {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			av, inA := am[k]
			bv, inB := bm[k]
			switch {
			case inA && !inB:
				d.op("remove", escapePointer(k), nil, false)
			case !inA && inB:
				d.op("add", escapePointer(k), bv, true)
			default:
				d.child(escapePointer(k), av, bv)
			}
		}
		return
	}
	aa, aIsArr := a.([]any)
	ba, bIsArr := b.([]any)
	if aIsArr && bIsArr {
		n := min(len(aa), len(ba))
		for i := 0; i < n; i++ {
			d.child(strconv.Itoa(i), aa[i], ba[i])
		}
		for i := n; i < len(ba); i++ {
			d.op("add", strconv.Itoa(i), ba[i], true)
		}
		for i := len(aa) - 1; i >= n; i-- {
			d.op("remove", strconv.Itoa(i), nil, false)
		}
		return
	}
	if !aIsMap && !bIsMap && !aIsArr && !bIsArr && bytes.Equal(encodeTree(a), encodeTree(b)) {
		return
	}
	if d.over || !d.spend("") {
		return
	}
	*d.p = append(*d.p, Op{Op: "replace", Path: d.path(), Value: b})
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func unescapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

// ParsePatch reads an RFC 6902 patch from its JSON form, such as an event's Patch.
func ParsePatch(raw []byte) (Patch, error) {
	v, err := decodeJSON("patch", raw)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, errors.New("audit: the patch is not a JSON array")
	}
	p := make(Patch, 0, len(arr))
	for i, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("audit: patch operation %d is not an object", i)
		}
		op, _ := m["op"].(string)
		path, ok := m["path"].(string)
		if !ok {
			return nil, fmt.Errorf("audit: patch operation %d has no path", i)
		}
		p = append(p, Op{Op: op, Path: path, Value: m["value"]})
	}
	return p, nil
}

// Apply applies an RFC 6902 patch to a copy of doc and returns the result. It supports add, remove, replace and
// test, the operations that Diff writes.
func Apply(doc any, p Patch) (any, error) {
	root, err := normalize("document", doc)
	if err != nil {
		return nil, err
	}
	for i, o := range p {
		value, err := normalize("value", o.Value)
		if err != nil {
			return nil, err
		}
		switch o.Op {
		case "add":
			root, err = setAt(root, o.Path, value, true)
		case "replace":
			root, err = setAt(root, o.Path, value, false)
		case "remove":
			root, err = removeAt(root, o.Path)
		case "test":
			var got any
			got, err = valueAt(root, o.Path)
			if err == nil && !bytes.Equal(encodeTree(got), encodeTree(value)) {
				err = errors.New("the test value differs")
			}
		default:
			err = errors.New("the operation is not supported")
		}
		if err != nil {
			return nil, fmt.Errorf("audit: patch operation %d: %w", i, err)
		}
	}
	return root, nil
}

func splitPointer(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if path[0] != '/' {
		return nil, errors.New("the path is not a JSON pointer")
	}
	parts := strings.Split(path[1:], "/")
	for i, s := range parts {
		parts[i] = unescapePointer(s)
	}
	return parts, nil
}

func valueAt(root any, path string) (any, error) {
	parts, err := splitPointer(path)
	if err != nil {
		return nil, err
	}
	cur := root
	for _, k := range parts {
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[k]
			if !ok {
				return nil, fmt.Errorf("no member at %q", path)
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(c) {
				return nil, fmt.Errorf("no index at %q", path)
			}
			cur = c[i]
		default:
			return nil, fmt.Errorf("the path %q goes through a scalar", path)
		}
	}
	return cur, nil
}

// setAt sets the value at path; add inserts into an array, replace needs the member to exist.
func setAt(root any, path string, value any, add bool) (any, error) {
	parts, err := splitPointer(path)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return value, nil
	}
	parentPath := path[:strings.LastIndex(path, "/")]
	parent, err := valueAt(root, parentPath)
	if err != nil {
		return nil, err
	}
	last := parts[len(parts)-1]
	switch c := parent.(type) {
	case map[string]any:
		if _, ok := c[last]; !ok && !add {
			return nil, fmt.Errorf("no member to replace at %q", path)
		}
		c[last] = value
		return root, nil
	case []any:
		i := len(c)
		if last != "-" {
			i, err = strconv.Atoi(last)
			if err != nil || i < 0 || i > len(c) || (!add && i == len(c)) {
				return nil, fmt.Errorf("no index at %q", path)
			}
		} else if !add {
			return nil, fmt.Errorf("no index to replace at %q", path)
		}
		var next []any
		if add {
			next = make([]any, 0, len(c)+1)
			next = append(next, c[:i]...)
			next = append(next, value)
			next = append(next, c[i:]...)
		} else {
			next = c
			next[i] = value
		}
		return setAt(root, parentPath, next, false)
	default:
		return nil, fmt.Errorf("the path %q goes through a scalar", path)
	}
}

func removeAt(root any, path string) (any, error) {
	parts, err := splitPointer(path)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, nil
	}
	parentPath := path[:strings.LastIndex(path, "/")]
	parent, err := valueAt(root, parentPath)
	if err != nil {
		return nil, err
	}
	last := parts[len(parts)-1]
	switch c := parent.(type) {
	case map[string]any:
		if _, ok := c[last]; !ok {
			return nil, fmt.Errorf("no member to remove at %q", path)
		}
		delete(c, last)
		return root, nil
	case []any:
		i, err := strconv.Atoi(last)
		if err != nil || i < 0 || i >= len(c) {
			return nil, fmt.Errorf("no index at %q", path)
		}
		next := append(append(make([]any, 0, len(c)-1), c[:i]...), c[i+1:]...)
		return setAt(root, parentPath, next, false)
	default:
		return nil, fmt.Errorf("the path %q goes through a scalar", path)
	}
}

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
	From  string
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
		case "move", "copy":
			m["from"] = o.From
		}
		out = append(out, m)
	}
	return out
}

// Diff returns the RFC 6902 patch from the redacted before to the redacted after (ST-SUP-RP05-LC05). A nil state
// is JSON null. A pii field whose value changed gets a replace with RedactionMarker, so the patch shows the change
// and not the value; a secret field is in no operation.
func Diff(before, after map[string]any, f Fields) (Patch, error) {
	rb, err := Redact(before, f)
	if err != nil {
		return nil, err
	}
	ra, err := Redact(after, f)
	if err != nil {
		return nil, err
	}
	var a, b any
	if rb != nil {
		a = rb
	}
	if ra != nil {
		b = ra
	}
	var p Patch
	diffValue("", a, b, &p)
	if rb != nil && ra != nil {
		var changed []string
		for k, s := range f {
			if s != PII {
				continue
			}
			ov, inBefore := before[k]
			nv, inAfter := after[k]
			if !inBefore || !inAfter {
				continue
			}
			ob, err := Canonical(ov)
			if err != nil {
				return nil, err
			}
			nb, err := Canonical(nv)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(ob, nb) {
				changed = append(changed, k)
			}
		}
		sort.Strings(changed)
		for _, k := range changed {
			p = append(p, Op{Op: "replace", Path: "/" + escapePointer(k), Value: RedactionMarker})
		}
	}
	return p, nil
}

func diffValue(path string, a, b any, p *Patch) {
	if bytes.Equal(encodeTree(a), encodeTree(b)) {
		return
	}
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
			child := path + "/" + escapePointer(k)
			av, inA := am[k]
			bv, inB := bm[k]
			switch {
			case inA && !inB:
				*p = append(*p, Op{Op: "remove", Path: child})
			case !inA && inB:
				*p = append(*p, Op{Op: "add", Path: child, Value: bv})
			default:
				diffValue(child, av, bv, p)
			}
		}
		return
	}
	aa, aIsArr := a.([]any)
	ba, bIsArr := b.([]any)
	if aIsArr && bIsArr {
		n := min(len(aa), len(ba))
		for i := 0; i < n; i++ {
			diffValue(path+"/"+strconv.Itoa(i), aa[i], ba[i], p)
		}
		for i := n; i < len(ba); i++ {
			*p = append(*p, Op{Op: "add", Path: path + "/" + strconv.Itoa(i), Value: ba[i]})
		}
		for i := len(aa) - 1; i >= n; i-- {
			*p = append(*p, Op{Op: "remove", Path: path + "/" + strconv.Itoa(i)})
		}
		return
	}
	*p = append(*p, Op{Op: "replace", Path: path, Value: b})
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func unescapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

// ParsePatch reads an RFC 6902 patch from its JSON form, such as an event's Patch.
func ParsePatch(raw []byte) (Patch, error) {
	v, err := decodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("audit: the patch is not JSON: %w", err)
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
		from, _ := m["from"].(string)
		p = append(p, Op{Op: op, Path: path, From: from, Value: m["value"]})
	}
	return p, nil
}

// Apply applies an RFC 6902 patch to a copy of doc and returns the result. It supports add, remove, replace and
// test, the operations that Diff writes.
func Apply(doc any, p Patch) (any, error) {
	root, err := normalize(doc)
	if err != nil {
		return nil, err
	}
	for i, o := range p {
		value, err := normalize(o.Value)
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
				err = fmt.Errorf("the value at %q differs", o.Path)
			}
		default:
			err = fmt.Errorf("the operation %q is not supported", o.Op)
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
		return nil, fmt.Errorf("the path %q is not a JSON pointer", path)
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
				return nil, fmt.Errorf("no member %q in %q", k, path)
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(c) {
				return nil, fmt.Errorf("no index %q in %q", k, path)
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
			return nil, fmt.Errorf("no member %q to replace", last)
		}
		c[last] = value
		return root, nil
	case []any:
		i := len(c)
		if last != "-" {
			i, err = strconv.Atoi(last)
			if err != nil || i < 0 || i > len(c) || (!add && i == len(c)) {
				return nil, fmt.Errorf("no index %q in %q", last, path)
			}
		} else if !add {
			return nil, fmt.Errorf("no index - to replace in %q", path)
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
			return nil, fmt.Errorf("no member %q to remove", last)
		}
		delete(c, last)
		return root, nil
	case []any:
		i, err := strconv.Atoi(last)
		if err != nil || i < 0 || i >= len(c) {
			return nil, fmt.Errorf("no index %q in %q", last, path)
		}
		next := append(append(make([]any, 0, len(c)-1), c[:i]...), c[i+1:]...)
		return setAt(root, parentPath, next, false)
	default:
		return nil, fmt.Errorf("the path %q goes through a scalar", path)
	}
}

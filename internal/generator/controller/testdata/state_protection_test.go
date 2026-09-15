package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestStateProtectionBindingAndCorruption(t *testing.T) {
	key := StateKey{Instance: "tenant", Entity: "Record", ResourceID: "resource", Scope: "provider"}
	master := bytes.Repeat([]byte{1}, 32)
	p, err := NewStateProtector([][]byte{master})
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("private provider migration checkpoint")
	sealed, err := p.Seal(key, 1, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plain) {
		t.Fatal("state stored plaintext")
	}
	again, err := p.Seal(key, 1, plain)
	if err != nil || bytes.Equal(sealed, again) {
		t.Fatal("sealing reused its envelope", err)
	}
	value, err := p.Open(key, 1, sealed)
	if err != nil || !bytes.Equal(value.Reveal(), plain) {
		t.Fatal("round trip failed", err)
	}
	copyValue := value.Reveal()
	copyValue[0] = 0
	if !bytes.Equal(value.Reveal(), plain) {
		t.Fatal("plaintext alias escaped")
	}
	for _, changed := range []StateKey{
		{"other", key.Entity, key.ResourceID, key.Scope}, {key.Instance, "Other", key.ResourceID, key.Scope},
		{key.Instance, key.Entity, "other", key.Scope}, {key.Instance, key.Entity, key.ResourceID, "other"},
	} {
		if got, err := p.Open(changed, 1, sealed); err == nil || len(got.Reveal()) != 0 {
			t.Fatal("cross-key state accepted")
		}
	}
	if _, err := p.Open(key, 2, sealed); err == nil {
		t.Fatal("old record version accepted")
	}
	for i := range sealed {
		altered := bytes.Clone(sealed)
		altered[i] ^= 1
		if got, err := p.Open(key, 1, altered); err == nil || len(got.Reveal()) != 0 {
			t.Fatal("damaged state accepted", i)
		}
	}
	for _, object := range []any{p, *p, value} {
		if strings.Contains(fmt.Sprintf("%v %+v %#v", object, object, object), string(plain)) {
			t.Fatal("formatting disclosed state")
		}
		if _, err := json.Marshal(object); err == nil {
			t.Fatal("implicit JSON export accepted")
		}
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%d", "%x", "%s", "%q"} {
		if fmt.Sprintf(format, p) != "[protected state keys]" {
			t.Fatal("key formatting is not redacted")
		}
		if fmt.Sprintf(format, value) != "[protected state]" {
			t.Fatal("state formatting is not redacted")
		}
	}
}

func TestStateProtectionRotationAndLimits(t *testing.T) {
	oldKey, newKey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	key := StateKey{"tenant", "Record", "resource", "provider"}
	old, _ := NewStateProtector([][]byte{oldKey})
	rotated, _ := NewStateProtector([][]byte{newKey, oldKey})
	current, _ := NewStateProtector([][]byte{newKey})
	oldKey[0] = 3
	newKey[0] = 4
	sealed, err := old.Seal(key, 1, []byte("checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotated.Open(key, 1, sealed); err != nil {
		t.Fatal("rotation lost old state", err)
	}
	if _, err := current.Open(key, 1, sealed); err == nil {
		t.Fatal("removed key still decrypts")
	}
	next, err := rotated.Seal(key, 2, []byte("checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := current.Open(key, 2, next); err != nil {
		t.Fatal("rotation did not select the active key", err)
	}
	if _, err := old.Open(key, 2, next); err == nil {
		t.Fatal("old key decrypts new state")
	}
	maximum := bytes.Repeat([]byte{255}, MaxStatePlaintextBytes)
	sealed, err = current.Seal(key, 3, maximum)
	if err != nil || len(sealed) != MaxProtectedStateBytes {
		t.Fatal("maximum state bound differs", err)
	}
	if got, err := current.Open(key, 3, sealed); err != nil || !bytes.Equal(got.Reveal(), maximum) {
		t.Fatal("maximum state lost bytes", err)
	}
	if _, err := current.Seal(key, 3, append(maximum, 0)); err == nil {
		t.Fatal("oversized state accepted")
	}
	for _, keys := range [][][]byte{nil, {make([]byte, 31)}, {make([]byte, 32)}, {oldKey, oldKey}, {oldKey, newKey, oldKey, newKey, oldKey}} {
		if _, err := NewStateProtector(keys); err == nil {
			t.Fatal("invalid keys accepted")
		}
	}
	for _, input := range []struct {
		key     StateKey
		version int64
	}{
		{key, 0}, {StateKey{}, 1}, {StateKey{"tenant", "Record", "resource", "\x00"}, 1},
		{StateKey{"tenant", "Record", strings.Repeat("a", 257), "provider"}, 1},
	} {
		if _, err := current.Seal(input.key, input.version, nil); err == nil {
			t.Fatal("invalid state identity accepted")
		}
	}
	for _, bad := range [][]byte{nil, {1}, make([]byte, MaxProtectedStateBytes+1)} {
		if _, err := current.Open(key, 1, bad); err == nil {
			t.Fatal("invalid envelope accepted")
		}
	}
	var zero StateProtector
	if _, err := zero.Seal(key, 1, nil); err == nil {
		t.Fatal("zero protector accepted")
	}
}

func TestStateProtectionConcurrentUse(t *testing.T) {
	p, _ := NewStateProtector([][]byte{bytes.Repeat([]byte{3}, 32)})
	var wg sync.WaitGroup
	for n := int64(1); n <= 8; n++ {
		wg.Add(1)
		go func(version int64) {
			defer wg.Done()
			key := StateKey{"tenant", "Record", "resource", "provider"}
			sealed, err := p.Seal(key, version, []byte("checkpoint"))
			if err != nil {
				t.Error(err)
				return
			}
			value, err := p.Open(key, version, sealed)
			if err != nil || string(value.Reveal()) != "checkpoint" {
				t.Error("concurrent state failed", err)
			}
		}(n)
	}
	wg.Wait()
}

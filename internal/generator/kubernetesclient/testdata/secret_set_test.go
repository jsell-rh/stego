package kubernetes

import (
	"errors"
	"strings"
	"testing"
)

func secretSetFixture() ([]string, Owner, []Object) {
	names := []string{"runtime", "files"}
	owner := Owner{"example.test/owner": "widget"}
	secrets := make([]Object, len(names))
	for i, name := range names {
		secrets[i] = Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"namespace": "service", "name": name, "uid": "uid", "resourceVersion": "1", "labels": Object{"example.test/owner": "widget"}}, "data": Object{"value": "YWJj"}}
	}
	return names, owner, secrets
}
func TestOpaqueSecretSetTracksDataAndIdentity(t *testing.T) {
	names, owner, secrets := secretSetFixture()
	first, err := OpaqueSecretSetDigest("service", names, owner, secrets)
	if err != nil || len(first) != 64 {
		t.Fatal("valid Secrets were rejected", err)
	}
	secrets[0]["metadata"].(Object)["resourceVersion"] = "2"
	same, err := OpaqueSecretSetDigest("service", names, owner, secrets)
	if err != nil || same != first {
		t.Fatal("metadata changed the digest", err)
	}
	secrets[0]["data"].(Object)["value"] = "ZGVm"
	changed, err := OpaqueSecretSetDigest("service", names, owner, secrets)
	if err != nil || changed == first {
		t.Fatal("data did not change the digest", err)
	}
	names[0] = "replacement"
	secrets[0]["metadata"].(Object)["name"] = "replacement"
	renamed, err := OpaqueSecretSetDigest("service", names, owner, secrets)
	if err != nil || renamed == changed {
		t.Fatal("name did not change the digest", err)
	}
	// API decoding and typed Object construction must produce the same hash.
	secrets[0]["data"] = map[string]any{"value": "ZGVm"}
	same, err = OpaqueSecretSetDigest("service", names, owner, secrets)
	if err != nil || same != renamed {
		t.Fatal("map representation changed the digest", err)
	}
}
func TestOpaqueSecretSetRejectsInvalidInput(t *testing.T) {
	cases := map[string]func([]string, Owner, []Object){
		"owner":               func(n []string, o Owner, s []Object) { o["example.test/owner"] = "other" },
		"uid":                 func(n []string, o Owner, s []Object) { delete(s[0]["metadata"].(Object), "uid") },
		"revision":            func(n []string, o Owner, s []Object) { delete(s[0]["metadata"].(Object), "resourceVersion") },
		"namespace":           func(n []string, o Owner, s []Object) { s[0]["metadata"].(Object)["namespace"] = "foreign" },
		"name":                func(n []string, o Owner, s []Object) { s[0]["metadata"].(Object)["name"] = "foreign" },
		"deleted":             func(n []string, o Owner, s []Object) { s[0]["metadata"].(Object)["deletionTimestamp"] = "now" },
		"invalid deletion":    func(n []string, o Owner, s []Object) { s[0]["metadata"].(Object)["deletionTimestamp"] = 1 },
		"kind":                func(n []string, o Owner, s []Object) { s[0]["kind"] = "ConfigMap" },
		"version":             func(n []string, o Owner, s []Object) { s[0]["apiVersion"] = "v2" },
		"type":                func(n []string, o Owner, s []Object) { s[0]["type"] = "kubernetes.io/tls" },
		"empty":               func(n []string, o Owner, s []Object) { s[0]["data"] = Object{} },
		"key":                 func(n []string, o Owner, s []Object) { s[0]["data"] = Object{"../key": "YWJj"} },
		"value type":          func(n []string, o Owner, s []Object) { s[0]["data"] = Object{"key": 1} },
		"invalid base64":      func(n []string, o Owner, s []Object) { s[0]["data"] = Object{"key": "%%%"} },
		"base64 newline":      func(n []string, o Owner, s []Object) { s[0]["data"] = Object{"key": "YWJj\n"} },
		"base64 padding bits": func(n []string, o Owner, s []Object) { s[0]["data"] = Object{"key": "YR=="} },
		"entry bound":         func(n []string, o Owner, s []Object) { s[0]["data"] = Object{"key": strings.Repeat("a", (128<<10)+1)} },
		"total bound": func(n []string, o Owner, s []Object) {
			s[0]["data"] = Object{"a": strings.Repeat("a", 128<<10), "b": strings.Repeat("a", 128<<10), "c": strings.Repeat("a", 128<<10), "d": strings.Repeat("a", 128<<10)}
		},
		"duplicate names": func(n []string, o Owner, s []Object) { n[1] = n[0] },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			n, o, s := secretSetFixture()
			change(n, o, s)
			digest, err := OpaqueSecretSetDigest("service", n, o, s)
			if !errors.Is(err, ErrResourceObservation) || digest != "" {
				t.Fatal("invalid Secret set accepted", err)
			}
		})
	}
	n, o, s := secretSetFixture()
	for _, namespace := range []string{"", "../foreign"} {
		if _, err := OpaqueSecretSetDigest(namespace, n, o, s); err == nil {
			t.Fatal("invalid namespace accepted")
		}
	}
	if _, err := OpaqueSecretSetDigest("service", nil, o, nil); err == nil {
		t.Fatal("empty set accepted")
	}
	if _, err := OpaqueSecretSetDigest("service", n, o, s[:1]); err == nil {
		t.Fatal("missing Secret accepted")
	}
	if _, err := OpaqueSecretSetDigest("service", n, nil, s); err == nil {
		t.Fatal("missing owner accepted")
	}
}

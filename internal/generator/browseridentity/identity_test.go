package browseridentity

import (
	"reflect"
	"strings"
	"testing"
)

func TestSharedIdentity(t *testing.T) {
	for _, test := range []struct {
		name, service   string
		client, backend map[string]any
		want            string
	}{
		{"default", "example-browser", nil, nil, "example-browser"},
		{"client", "example-browser", map[string]any{"service_name": "selected"}, nil, "selected"},
		{"backend", "example-browser", nil, map[string]any{"telemetry_service_name": "selected"}, "selected"},
		{"both", "example-browser", map[string]any{"service_name": "selected"}, map[string]any{"telemetry_service_name": "selected"}, "selected"},
		{"distinct service", "", map[string]any{"service_name": "selected"}, nil, "selected"},
		{"conflict", "example-browser", map[string]any{"service_name": "first"}, map[string]any{"telemetry_service_name": "second"}, ""},
		{"missing", "", nil, nil, ""},
		{"invalid default", "invalid name", nil, nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := Resolve(test.service, test.client, test.backend)
			if got != test.want || (err != nil) != (test.want == "") {
				t.Fatalf("Resolve() = %q, %v", got, err)
			}
		})
	}
}

func TestInvalidExplicitIdentityCannotSelectDefault(t *testing.T) {
	for _, value := range []any{nil, "", "invalid name", "upperCase", "with\nline", strings.Repeat("a", 65), 1, false} {
		for _, client := range []bool{false, true} {
			left, right := map[string]any{}, map[string]any{}
			if client {
				left["service_name"] = value
			} else {
				right["telemetry_service_name"] = value
			}
			if _, err := Resolve("valid-default", left, right); err == nil {
				t.Fatal("invalid explicit identity selected a default")
			}
		}
	}
	client := map[string]any{"service_name": "selected"}
	backend := map[string]any{"routes": []any{"/"}}
	if _, err := Resolve("service", client, backend); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client, map[string]any{"service_name": "selected"}) || !reflect.DeepEqual(backend, map[string]any{"routes": []any{"/"}}) {
		t.Fatal("identity resolution changed component declarations")
	}
}

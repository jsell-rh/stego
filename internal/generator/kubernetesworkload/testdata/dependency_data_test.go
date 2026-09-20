package workload

import (
	"bytes"
	"encoding/base64"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestWidgetDependencyDataPreservesConstruction(t *testing.T) {
	want := widget()
	got := widget()
	inputs := make([]map[string]any, len(want.Dependencies))
	for i, dependency := range want.Dependencies {
		values := map[string]any{}
		for key, raw := range dependency.Data {
			if dependency.Kind == "Secret" {
				values[key] = base64.StdEncoding.EncodeToString(raw)
			} else {
				values[key] = string(raw)
			}
		}
		inputs[i] = values
		converted, err := DependencyFromData(dependency.Kind, dependency.Name, values)
		if err != nil {
			t.Fatal(err)
		}
		got.Dependencies[i] = converted
	}
	if !reflect.DeepEqual(want.Dependencies, got.Dependencies) {
		t.Fatal("converted contents differ")
	}
	for _, values := range inputs {
		for key := range values {
			values[key] = "changed source"
		}
	}
	if !reflect.DeepEqual(render(t, want), render(t, got)) {
		t.Fatal("conversion or source mutation changed construction")
	}
	for _, values := range inputs {
		values["new"] = "new source"
	}
	if !reflect.DeepEqual(render(t, want), render(t, got)) {
		t.Fatal("source map is shared")
	}
	before, err := ConfigurationDigest(got.Dependencies)
	if err != nil {
		t.Fatal(err)
	}
	got.Dependencies[0].Data["uri"][0] ^= 1
	after, err := ConfigurationDigest(got.Dependencies)
	if err != nil || after == before {
		t.Fatal("changed decoded contents did not change the digest")
	}
	if !bytes.Equal(want.Dependencies[0].Data["uri"], []byte("private-database-value")) {
		t.Fatal("converted bytes are shared")
	}
}

func TestDependencyDataRejectsInvalidInput(t *testing.T) {
	many := map[string]any{}
	for i := 0; i < 257; i++ {
		many[strconv.Itoa(i)] = ""
	}
	cases := []struct {
		name, kind, objectName string
		data                   map[string]any
	}{
		{"kind", "Other", "widget", map[string]any{"key": "private"}},
		{"name", "Secret", "../private", map[string]any{"key": "cHJpdmF0ZQ=="}},
		{"nil", "Secret", "widget", nil},
		{"empty", "ConfigMap", "widget", map[string]any{}},
		{"entries", "ConfigMap", "widget", many},
		{"empty-key", "Secret", "widget", map[string]any{"": "YQ=="}},
		{"path-key", "Secret", "widget", map[string]any{"../private": "YQ=="}},
		{"long-key", "Secret", "widget", map[string]any{strings.Repeat("a", 254): "YQ=="}},
		{"value-type", "Secret", "widget", map[string]any{"key": []byte("private")}},
		{"invalid-text", "ConfigMap", "widget", map[string]any{"key": string([]byte{0xff})}},
		{"invalid-base64", "Secret", "widget", map[string]any{"key": "private!"}},
		{"padding-bits", "Secret", "widget", map[string]any{"key": "YR=="}},
		{"missing-padding", "Secret", "widget", map[string]any{"key": "YQ"}},
		{"url-alphabet", "Secret", "widget", map[string]any{"key": "-w=="}},
		{"line-feed", "Secret", "widget", map[string]any{"key": "YQ==\n"}},
		{"carriage-return", "Secret", "widget", map[string]any{"key": "YQ==\r"}},
		{"encoded-limit", "Secret", "widget", map[string]any{"key": strings.Repeat("A", (2<<20)+1)}},
		{"decoded-limit", "Secret", "widget", map[string]any{"key": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), 1<<20))}},
		{"text-limit", "ConfigMap", "widget", map[string]any{"key": strings.Repeat("a", 1<<20)}},
		{"aggregate-limit", "ConfigMap", "widget", map[string]any{"first": strings.Repeat("a", 1<<19), "second": strings.Repeat("b", 1<<19)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, err := DependencyFromData(tc.kind, tc.objectName, tc.data)
			if !errors.Is(err, ErrDeclaration) || !reflect.DeepEqual(value, Dependency{}) {
				t.Fatal("invalid data returned a partial dependency or no fixed error")
			}
			if err.Error() != ErrDeclaration.Error() {
				t.Fatal("error contains input details")
			}
		})
	}
}

func TestDependencyDataBoundsAndEmptyValues(t *testing.T) {
	for _, kind := range []string{"ConfigMap", "Secret"} {
		t.Run(kind, func(t *testing.T) {
			// The public bound includes the object kind, name, key, and decoded bytes.
			raw := bytes.Repeat([]byte("a"), (1<<20)-len(kind)-len("widget")-len("key"))
			encode := func(value []byte) string {
				if kind == "Secret" {
					return base64.StdEncoding.EncodeToString(value)
				}
				return string(value)
			}
			got, err := DependencyFromData(kind, "widget", map[string]any{"key": encode(raw)})
			if err != nil || !bytes.Equal(got.Data["key"], raw) {
				t.Fatal("the valid boundary was rejected", err)
			}
			digest, err := ConfigurationDigest([]Dependency{got})
			expected, expectedErr := ConfigurationDigest([]Dependency{{Kind: kind, Name: "widget", Data: map[string][]byte{"key": raw}}})
			if err != nil || expectedErr != nil || digest != expected {
				t.Fatal("conversion changed the digest")
			}
			extra := append(append([]byte(nil), raw...), 'b')
			if value, err := DependencyFromData(kind, "widget", map[string]any{"key": encode(extra)}); err != ErrDeclaration || !reflect.DeepEqual(value, Dependency{}) {
				t.Fatal("the size bound was not enforced")
			}
			empty, err := DependencyFromData(kind, "widget", map[string]any{"key": ""})
			if err != nil || len(empty.Data) != 1 || len(empty.Data["key"]) != 0 {
				t.Fatal("an empty value was rejected")
			}
		})
	}
	raw := []byte{0, 255, '\n'}
	got, err := DependencyFromData("Secret", "widget", map[string]any{"binary": base64.StdEncoding.EncodeToString(raw)})
	if err != nil || !bytes.Equal(got.Data["binary"], raw) {
		t.Fatal("binary Secret data changed")
	}
}

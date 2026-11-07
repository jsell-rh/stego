package compiler

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestInvalidComponentOverridesGateCompilation(t *testing.T) {
	t.Run("ST-GEN-INV32-T A setting of a component outside the active set, or one its schema refuses, stops checking and generation", func(t *testing.T) {
		testInvalidComponentOverridesGateCompilation(t)
	})
}

func testInvalidComponentOverridesGateCompilation(t *testing.T) {
	for _, override := range []string{
		"stub-api: {heder: Authorization}",
		"stub-api: {header: 123}",
		"absent: {header: Authorization}",
		"stub-api: false",
	} {
		t.Run(override, func(t *testing.T) {
			project, registry, input := setupValidateProject(t)
			writeFile(t, filepath.Join(registry, "components/stub-api/component.yaml"), `kind: component
name: stub-api
version: 1.0.0
stage: ga
output_namespace: internal/api
config:
  header: {type: string, default: Authorization}
requires: [storage-adapter]
provides: [http-server]
`)
			writeFile(t, filepath.Join(project, "service.yaml"), "kind: service\nname: test\narchetype: test-arch\nlanguage: go\noverrides:\n  "+override+"\n")
			validation, err := Validate(input)
			if err != nil || !validation.HasErrors() {
				t.Fatalf("invalid configuration was accepted: result=%+v err=%v", validation, err)
			}
			plan, err := Reconcile(input)
			if plan != nil || err == nil || !strings.Contains(err.Error(), "[config]") {
				t.Fatalf("invalid configuration reached generation: plan=%+v err=%v", plan, err)
			}
		})
	}
}

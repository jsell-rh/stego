package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
)

// Use the registry schema and normal compiler path, not only the generator.
func TestAllocationCompilation(t *testing.T) {
	project := t.TempDir()
	registry := filepath.Join(project, "registry")
	if err := os.CopyFS(registry, os.DirFS("../../registry")); err != nil {
		t.Fatal(err)
	}
	archetype := filepath.Join(registry, "archetypes/rest-crud/archetype.yaml")
	data, err := os.ReadFile(archetype)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "components:\n", "components:\n  - controller\n  - kubernetes-client\n  - kubernetes-service\n", 1))
	data = []byte(strings.Replace(string(data), "  - rest-api", "  - http-application", 1))
	data = []byte(strings.Replace(string(data), "bindings:\n", "bindings:\n  application-http: http-application\n  health-endpoint: health-check\n", 1))
	if err := os.WriteFile(archetype, data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "internal/worker"), 0755); err != nil {
		t.Fatal(err)
	}
	worker := `package worker
import("context"; runtime "example.com/widget/out/controller")
func Run(ctx context.Context, metrics *runtime.Metrics) error { <-ctx.Done(); return nil }
`
	if err := os.WriteFile(filepath.Join(project, "internal/worker/worker.go"), []byte(worker), 0644); err != nil {
		t.Fatal(err)
	}
	declaration := `kind: service
name: widget
archetype: rest-crud
language: go
entities:
  - name: Widget
    fields: [{name: label, type: string}]
collections:
  widgets: {entity: Widget, operations: [create, read]}
overrides:
  jwt-auth: {mode: verifier}
  http-application: {factory_package: internal/worker}
  controller: {}
  kubernetes-client: {}
  kubernetes-service:
    source_directories: [internal]
    allocation_roles:
      - name: data
        scope: namespace
        rules: [{api_group: "", resources: [secrets], verbs: [get]}]
    allocation_profiles:
      - name: tenant
        namespace_prefix: tenant-
        suffix_length: 8
        owner_label: example.test/owner
        manager: widget
        quota:
          - {resource: pods, value: "1"}
          - {resource: limits.cpu, value: "1"}
          - {resource: limits.memory, value: 256Mi}
          - {resource: limits.ephemeral-storage, value: 128Mi}
          - {resource: requests.storage, value: 1Gi}
        bindings:
          - {role: data, service_account: operator, namespace: external, external_namespace: operator-system}
    workers:
      - name: allocator
        package: internal/worker
        function: Run
        namespace_allocator: true
        kubernetes_api: true
        external_endpoints: [kubernetes]
      - name: data
        package: internal/worker
        function: Run
        network_peers:
          - {direction: egress, allocation_profile: tenant, pod_label: app, pod_value: database, port: 5432, protocol: TCP}
`
	input := compiler.ReconcilerInput{ProjectDir: project, RegistryDir: registry, GoVersion: "1.26.8", ModuleName: "example.com/widget", Generators: defaultGenerators()}
	for name, source := range map[string]string{
		"valid":                      declaration,
		"both selectors":             strings.Replace(declaration, "allocation_profile: tenant,", "allocation_profile: tenant, namespace: self,", 1),
		"unknown profile":            strings.Replace(declaration, "allocation_profile: tenant,", "allocation_profile: absent,", 1),
		"missing external namespace": strings.Replace(declaration, ", external_namespace: operator-system", "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(project, "service.yaml"), []byte(source), 0644); err != nil {
				t.Fatal(err)
			}
			plan, err := compiler.Reconcile(input)
			if name != "valid" {
				if err == nil || plan != nil {
					t.Fatal("invalid allocation declaration reached generation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range plan.Files {
				if strings.HasSuffix(f.Path, "deploy/render/worker-data.json.tmpl") {
					found = true
				}
			}
			if !found {
				t.Fatal("allocation worker manifest missing")
			}
		})
	}
}

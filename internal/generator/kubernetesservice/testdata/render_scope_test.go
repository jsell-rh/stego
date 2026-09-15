package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDeploymentScopePartition(t *testing.T) {
	base := []string{"--image", "registry.example.test/team/widget@sha256:" + strings.Repeat("a", 64), "--namespace", "test"}
	seen := map[string]bool{}
	for _, target := range [][]string{{"--egress", "kubernetes=10.0.0.1:443"}, {"--worker", "queue", "--egress", "kubernetes=10.0.0.1:443"}, {"--rpc-process", "records"}} {
		args := append(append([]string{}, base...), target...)
		var original bytes.Buffer
		if err := render(args, &original); err != nil {
			t.Fatal(err)
		}
		var complete struct{ Items []map[string]any }
		if err := json.Unmarshal(original.Bytes(), &complete); err != nil {
			t.Fatal(err)
		}
		for _, scope := range []string{"all", "cluster", "namespace"} {
			selected := append(append([]string{}, args...), "--scope", scope)
			var output, repeated bytes.Buffer
			if err := render(selected, &output); err != nil {
				t.Fatal(err)
			}
			if err := render(selected, &repeated); err != nil || !bytes.Equal(output.Bytes(), repeated.Bytes()) {
				t.Fatal("scoped output differs", err)
			}
			if scope == "all" && !bytes.Equal(original.Bytes(), output.Bytes()) {
				t.Fatal("default output changed")
			}
			var document struct {
				APIVersion string
				Kind       string
				Items      []map[string]any
			}
			if err := json.Unmarshal(output.Bytes(), &document); err != nil || document.APIVersion != "v1" || document.Kind != "List" || document.Items == nil {
				t.Fatal("invalid scoped list", err)
			}
			want := make([]map[string]any, 0, len(complete.Items))
			for _, item := range complete.Items {
				seen[item["kind"].(string)] = true
				namespace, _ := item["metadata"].(map[string]any)["namespace"].(string)
				if scope == "all" || scope == "cluster" && namespace == "" || scope == "namespace" && namespace == "test" {
					want = append(want, item)
				}
			}
			if !reflect.DeepEqual(want, document.Items) {
				t.Fatal("scope changed resources, order, or permissions", scope)
			}
		}
		for _, scope := range []string{"", "unknown", "Cluster"} {
			var output bytes.Buffer
			if err := render(append(append([]string{}, args...), "--scope", scope), &output); err == nil || output.Len() != 0 {
				t.Fatal("invalid scope emitted output")
			}
		}
	}
	for _, kind := range []string{"Deployment", "Service", "ServiceAccount", "NetworkPolicy", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
		if !seen[kind] {
			t.Fatal("scope check omitted a generated kind", kind)
		}
	}
	for _, scope := range []string{"cluster", "namespace"} {
		var output bytes.Buffer
		if err := render(append(append([]string{}, base...), "--worker", "queue", "--scope", scope), &output); err == nil || output.Len() != 0 {
			t.Fatal("scope bypassed endpoint validation")
		}
	}
}

func TestDeploymentScopeRejectsUnknownOrWrongNamespace(t *testing.T) {
	for _, item := range []map[string]any{
		{"apiVersion": "custom.test/v1", "kind": "Deployment", "metadata": map[string]string{"name": "item", "namespace": "test"}},
		{"apiVersion": "v1", "kind": "Unknown", "metadata": map[string]string{"name": "item", "namespace": "test"}},
		{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]string{"name": "item"}},
		{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]string{"name": "item", "namespace": "other"}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": map[string]string{"name": "item", "namespace": "test"}},
		{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]string{"namespace": "test"}},
	} {
		input, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{item}})
		if err != nil {
			t.Fatal(err)
		}
		for _, scope := range []string{"cluster", "namespace"} {
			if output, err := selectDeploymentScope(input, scope, "test"); err == nil || len(output) != 0 {
				t.Fatal("invalid generated resource passed scope selection")
			}
		}
	}
}

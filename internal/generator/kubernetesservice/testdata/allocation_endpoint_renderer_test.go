package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAllocatedEndpointRendering(t *testing.T) {
	namespace := os.Getenv("STEGO_ALLOCATION_NAMESPACE")
	if namespace == "" {
		namespace = "control"
	}
	base := []string{"--image", "registry.test/widget@sha256:" + strings.Repeat("a", 64), "--namespace", namespace}
	endpoints := []string{"--egress", "kubernetes=192.0.2.10:443", "--egress", "provider=[2001:db8::1]:5432", "--egress", "provider=192.0.2.10:443"}
	for _, worker := range []string{"queue", "data"} {
		args := append(append([]string{}, base...), "--worker", worker)
		args = append(args, endpoints...)
		var output bytes.Buffer
		if err := render(args, &output); err != nil {
			t.Fatal(err)
		}
		if dir := os.Getenv("STEGO_ALLOCATION_ENDPOINT_ARTIFACTS"); dir != "" && worker == "queue" {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), output.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			nextArgs := append(append([]string{}, base...), "--worker", "queue", "--egress", "kubernetes=192.0.2.11:443", "--egress", "provider=192.0.2.12:5432")
			var next bytes.Buffer
			if err := render(nextArgs, &next); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "next-manifest.json"), next.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if strings.Contains(output.String(), "stego_unrendered_allocation_endpoints") {
			t.Fatal("endpoint placeholder remains")
		}
		var doc struct{ Items []map[string]any }
		if err := json.Unmarshal(output.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		deployment, admission := false, false
		for _, item := range doc.Items {
			switch item["kind"] {
			case "Deployment":
				containers := item["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
				for _, raw := range containers[0].(map[string]any)["env"].([]any) {
					entry := raw.(map[string]any)
					if entry["name"] != "STEGO_ALLOCATION_NETWORK_ENDPOINTS" {
						continue
					}
					var bindings map[string][]string
					if err := json.Unmarshal([]byte(entry["value"].(string)), &bindings); err != nil {
						t.Fatal(err)
					}
					expected := map[string][]string{"kubernetes": {"192.0.2.10:443"}, "provider": {"192.0.2.10:443", "[2001:db8::1]:5432"}}
					if !reflect.DeepEqual(bindings, expected) {
						t.Fatal("runtime endpoint bindings differ", bindings)
					}
					deployment = true
				}
			case "ValidatingAdmissionPolicy":
				variables := item["spec"].(map[string]any)["variables"].([]any)
				for _, raw := range variables {
					v := raw.(map[string]any)
					if v["name"] != "networkEndpoints" {
						continue
					}
					var profiles map[string][]map[string]any
					if err := json.Unmarshal([]byte(v["expression"].(string)), &profiles); err != nil {
						t.Fatal(err)
					}
					expected := []map[string]any{{"cidr": "192.0.2.10/32", "port": "443"}, {"cidr": "2001:db8::1/128", "port": "5432"}}
					if !reflect.DeepEqual(profiles["tenant"], expected) || len(profiles) != 1 {
						t.Fatal("admission did not deduplicate exact endpoints", profiles)
					}
					admission = true
				}
			}
		}
		if !deployment || admission != (worker == "queue") {
			t.Fatal("endpoint configuration reached the wrong resources", worker)
		}
		reversed := append(append([]string{}, base...), "--worker", worker)
		for i := len(endpoints) - 2; i >= 0; i -= 2 {
			reversed = append(reversed, endpoints[i:i+2]...)
		}
		var second bytes.Buffer
		if err := render(reversed, &second); err != nil || !bytes.Equal(output.Bytes(), second.Bytes()) {
			t.Fatal("flag order changed allocation output", err)
		}
	}
	var output bytes.Buffer
	if err := render(append(append([]string{}, base...), "--worker", "other"), &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "STEGO_ALLOCATION_NETWORK_ENDPOINTS") {
		t.Fatal("unrelated worker received endpoint configuration")
	}
	for _, extra := range [][]string{
		{"--egress", "kubernetes=192.0.2.10:443"},
		{"--egress", "kubernetes=192.0.2.10:443", "--egress", "provider=127.0.0.1:443"},
		{"--egress", "kubernetes=192.0.2.10:443", "--egress", "provider=database.example:5432"},
		append(append([]string{}, endpoints...), "--egress", "unknown=192.0.2.99:443"),
		append(append([]string{}, endpoints...), "--egress", "provider=192.0.2.10:443"),
	} {
		output.Reset()
		args := append(append([]string{}, base...), "--worker", "queue")
		args = append(args, extra...)
		if err := render(args, &output); err == nil || output.Len() != 0 {
			t.Fatal("invalid endpoint configuration emitted output", extra)
		}
	}
	output.Reset()
	args := append(append([]string{}, base...), "--worker", "other")
	args = append(args, endpoints...)
	if err := render(args, &output); err == nil || output.Len() != 0 {
		t.Fatal("unrelated workload accepted allocation endpoint bindings")
	}
}

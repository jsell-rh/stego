package kubernetesservice

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"text/template"
)

func TestAllocationNetworkPeer(t *testing.T) {
	c := allocationContext()
	w := c.ComponentConfig["workers"].([]any)[0].(object)
	w["network_peers"] = []any{object{"direction": "egress", "allocation_profile": "tenant", "pod_label": "app", "pod_value": "database", "port": 5432, "protocol": "TCP"}}
	files, _, err := new(Generator).Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"first-control", "second-control"} {
		found := false
		for _, f := range files {
			if f.Path != "deploy/render/worker-queue.json.tmpl" {
				continue
			}
			digest := sha256.Sum256([]byte(ns + ".widget-queue"))
			marker := hex.EncodeToString(digest[:16])
			tmpl, err := template.New("manifest").Funcs(template.FuncMap{"allocationID": func(namespace, allocator string) string {
				if namespace != ns || allocator != "widget-queue" {
					t.Fatal("network uses the wrong installation identity")
				}
				return marker
			}}).Parse(string(f.Content))
			if err != nil {
				t.Fatal(err)
			}
			var rendered bytes.Buffer
			if err := tmpl.Execute(&rendered, struct {
				Namespace, Image string
				FSGroup          int
			}{ns, "registry.test/widget@sha256:" + strings.Repeat("a", 64), 10001}); err != nil {
				t.Fatal(err)
			}
			var doc struct{ Items []object }
			if err := json.Unmarshal(rendered.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			for _, item := range doc.Items {
				if item["kind"] != "NetworkPolicy" {
					continue
				}
				rule := item["spec"].(object)["egress"].([]any)[0].(object)
				peer := rule["to"].([]any)[0].(object)
				labels := peer["namespaceSelector"].(object)["matchLabels"].(object)
				if len(labels) != 2 || labels["stego.dev/allocator"] != marker || labels["stego.dev/allocation-profile"] != "tenant" {
					t.Fatal("network does not restrict the allocation installation and profile")
				}
				pods := peer["podSelector"].(object)["matchLabels"].(object)
				port := rule["ports"].([]any)[0].(object)
				if len(pods) != 1 || pods["app"] != "database" || port["protocol"] != "TCP" || port["port"] != float64(5432) {
					t.Fatal("network lost its Pod or port restriction")
				}
				found = true
			}
		}
		if !found {
			t.Fatal("worker network policy missing")
		}
	}
}

func TestAllocationNetworkPeerRejectsInvalidSelectors(t *testing.T) {
	for name, selector := range map[string]object{
		"unknown profile":      {"allocation_profile": "other"},
		"empty profile":        {"allocation_profile": ""},
		"invalid profile type": {"allocation_profile": true},
		"both selectors":       {"allocation_profile": "tenant", "namespace": "self"},
		"missing selector":     {},
		"unknown selector":     {"namespace_label": "tenant"},
	} {
		t.Run(name, func(t *testing.T) {
			c := allocationContext()
			p := object{"direction": "egress", "pod_label": "app", "pod_value": "database", "port": 5432, "protocol": "TCP"}
			for k, v := range selector {
				p[k] = v
			}
			c.ComponentConfig["workers"].([]any)[0].(object)["network_peers"] = []any{p}
			if _, _, err := new(Generator).Generate(c); err == nil {
				t.Fatal("invalid network selector accepted")
			}
		})
	}
}

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
)

func admittedRoute(t *testing.T) Object {
	t.Helper()
	value, err := decodeObject([]byte(`{"apiVersion":"route.openshift.io/v1","kind":"Route","metadata":{"name":"public","namespace":"tenant","uid":"route-1","resourceVersion":"42","labels":{"example.com/owner":"tenant-1"}},"spec":{"host":"service.example.test","to":{"kind":"Service","name":"service","weight":100},"port":{"targetPort":"grpc"},"tls":{"termination":"passthrough","insecureEdgeTerminationPolicy":"None"},"wildcardPolicy":"None"},"status":{"ingress":[{"host":"service.example.test","routerName":"default","wildcardPolicy":"None","conditions":[{"type":"Admitted","status":"True"}]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func routeMap(o Object, keys ...string) map[string]any {
	value, _ := observationMap(Nested(o, keys...))
	return value
}
func routeIngress(o Object) []any             { return routeMap(o, "status")["ingress"].([]any) }
func selectedIngress(o Object) map[string]any { return routeIngress(o)[0].(map[string]any) }
func routeCondition(o Object) map[string]any {
	return selectedIngress(o)["conditions"].([]any)[0].(map[string]any)
}

func TestPassthroughRouteAdmission(t *testing.T) {
	target := PassthroughRouteTarget{Host: "service.example.test", Service: "service", Port: "grpc", Router: "default"}
	owner := Owner{"example.com/owner": "tenant-1"}
	for name, change := range map[string]func(Object){
		"admitted":       func(Object) {},
		"default weight": func(o Object) { delete(routeMap(o, "spec", "to"), "weight") },
		"duplicate agreeing ingress": func(o Object) {
			entries := routeIngress(o)
			routeMap(o, "status")["ingress"] = append(entries, entries[0])
		},
		"unselected router denial": func(o Object) {
			routeMap(o, "status")["ingress"] = append(routeIngress(o), map[string]any{"host": target.Host, "routerName": "private", "conditions": []any{map[string]any{"type": "Admitted", "status": "False"}}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := admittedRoute(t)
			change(o)
			if yes, err := PassthroughRouteAdmitted(o, owner, target); err != nil || !yes {
				t.Fatal("valid admission rejected", err)
			}
		})
	}
	for name, change := range map[string]func(Object){
		"missing status":    func(o Object) { delete(o, "status") },
		"no ingress":        func(o Object) { delete(routeMap(o, "status"), "ingress") },
		"empty ingress":     func(o Object) { routeMap(o, "status")["ingress"] = []any{} },
		"wrong router":      func(o Object) { selectedIngress(o)["routerName"] = "other" },
		"old host":          func(o Object) { selectedIngress(o)["host"] = "old.example.test" },
		"denied":            func(o Object) { routeCondition(o)["status"] = "False" },
		"unknown":           func(o Object) { routeCondition(o)["status"] = "Unknown" },
		"missing admission": func(o Object) { selectedIngress(o)["conditions"] = []any{} },
		"deleting":          func(o Object) { routeMap(o, "metadata")["deletionTimestamp"] = "2026-09-15T00:00:00Z" },
	} {
		t.Run(name, func(t *testing.T) {
			o := admittedRoute(t)
			change(o)
			if yes, err := PassthroughRouteAdmitted(o, owner, target); yes || err != nil {
				t.Fatal("pending admission not preserved", yes, err)
			}
		})
	}
	for name, change := range map[string]func(Object){
		"foreign owner":                func(o Object) { routeMap(o, "metadata")["labels"] = map[string]any{"example.com/owner": "other"} },
		"missing UID":                  func(o Object) { delete(routeMap(o, "metadata"), "uid") },
		"wrong kind":                   func(o Object) { o["kind"] = "Ingress" },
		"wrong host":                   func(o Object) { routeMap(o, "spec")["host"] = "other.example.test" },
		"wrong backend":                func(o Object) { routeMap(o, "spec", "to")["name"] = "other" },
		"wrong port":                   func(o Object) { routeMap(o, "spec", "port")["targetPort"] = "other" },
		"disabled backend":             func(o Object) { routeMap(o, "spec", "to")["weight"] = json.Number("0") },
		"fractional weight":            func(o Object) { routeMap(o, "spec", "to")["weight"] = json.Number("1.5") },
		"numeric string weight":        func(o Object) { routeMap(o, "spec", "to")["weight"] = "100" },
		"TLS termination":              func(o Object) { routeMap(o, "spec", "tls")["termination"] = "edge" },
		"HTTP enabled":                 func(o Object) { routeMap(o, "spec", "tls")["insecureEdgeTerminationPolicy"] = "Allow" },
		"router key":                   func(o Object) { routeMap(o, "spec", "tls")["key"] = "private-key" },
		"router certificate reference": func(o Object) { routeMap(o, "spec", "tls")["externalCertificate"] = map[string]any{"name": "other"} },
		"path routing":                 func(o Object) { routeMap(o, "spec")["path"] = "/other" },
		"wildcard":                     func(o Object) { routeMap(o, "spec")["wildcardPolicy"] = "Subdomain" },
		"alternate backend": func(o Object) {
			routeMap(o, "spec")["alternateBackends"] = []any{map[string]any{"kind": "Service", "name": "other"}}
		},
		"invalid status":           func(o Object) { o["status"] = true },
		"invalid ingress":          func(o Object) { routeMap(o, "status")["ingress"] = true },
		"too many ingress entries": func(o Object) { routeMap(o, "status")["ingress"] = make([]any, 65) },
		"too many conditions":      func(o Object) { selectedIngress(o)["conditions"] = make([]any, 65) },
		"invalid condition":        func(o Object) { routeCondition(o)["status"] = true },
		"duplicate condition": func(o Object) {
			entry := selectedIngress(o)
			conditions := entry["conditions"].([]any)
			entry["conditions"] = append(conditions, conditions[0])
		},
		"conflicting ingress": func(o Object) {
			routeMap(o, "status")["ingress"] = append(routeIngress(o), map[string]any{"host": target.Host, "routerName": target.Router, "conditions": []any{map[string]any{"type": "Admitted", "status": "False"}}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := admittedRoute(t)
			change(o)
			if yes, err := PassthroughRouteAdmitted(o, owner, target); yes || !errors.Is(err, ErrResourceObservation) {
				t.Fatal("invalid route was accepted", yes, err)
			}
		})
	}
	for _, host := range []string{"", "https://service.example.test", "*.example.test", "service.example.test.", "service_example.test", "127.0.0.1", "Service.example.test"} {
		invalid := target
		invalid.Host = host
		if yes, err := PassthroughRouteAdmitted(admittedRoute(t), owner, invalid); yes || !errors.Is(err, ErrResourceObservation) {
			t.Fatal("invalid host requirement accepted")
		}
	}
}

func TestPassthroughRouteEnsureRequiresStableProbeObservation(t *testing.T) {
	for _, mode := range []string{"ready", "pending", "probe failure", "replaced", "changed", "removed", "denied after probe", "foreign response", "nil probe", "invalid target", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			current := admittedRoute(t)
			target := PassthroughRouteTarget{Host: "service.example.test", Service: "service", Port: "grpc", Router: "default"}
			owner := Owner{"example.com/owner": "tenant-1"}
			if mode == "pending" {
				routeCondition(current)["status"] = "False"
			}
			if mode == "foreign response" {
				routeMap(current, "metadata")["namespace"] = "foreign"
			}
			var mu sync.Mutex
			calls := 0
			client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/apis/route.openshift.io/v1/namespaces/tenant/routes/public" {
					t.Error("unexpected Route request")
					w.WriteHeader(500)
					return
				}
				if calls == 2 {
					switch mode {
					case "replaced":
						routeMap(current, "metadata")["uid"] = "replacement"
					case "changed":
						routeMap(current, "metadata")["resourceVersion"] = "43"
					case "removed":
						w.WriteHeader(404)
						return
					case "denied after probe":
						routeCondition(current)["status"] = "False"
					}
				}
				_ = json.NewEncoder(w).Encode(current)
			})
			probes := 0
			probe := func(ctx context.Context, address string) error {
				probes++
				if address != "service.example.test:443" {
					t.Error("probe used an unselected host")
				}
				if mode == "probe failure" {
					return errors.New("probe failed")
				}
				return ctx.Err()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "nil probe" {
				probe = nil
			}
			if mode == "invalid target" {
				target.Host = "127.0.0.1"
			}
			if mode == "canceled" {
				cancel()
			}
			ready, err := client.EnsurePassthroughRoute(ctx, "tenant", "public", owner, target, probe)
			if mode == "ready" {
				if err != nil || !ready || probes != 1 {
					t.Fatal("verified Route rejected", err)
				}
			} else if ready {
				t.Fatal("unverified Route accepted")
			}
			mu.Lock()
			defer mu.Unlock()
			if (mode == "nil probe" || mode == "invalid target" || mode == "canceled") && calls != 0 {
				t.Fatal("invalid configuration reached Kubernetes")
			}
			if (mode == "pending" || mode == "foreign response") && probes != 0 {
				t.Fatal("unadmitted Route was probed")
			}
			if mode == "probe failure" && calls != 1 {
				t.Fatal("failed probe continued")
			}
			if (mode == "pending" || mode == "replaced" || mode == "changed" || mode == "removed" || mode == "denied after probe") && err != nil {
				t.Fatal("pending observation became an error", err)
			}
		})
	}
}

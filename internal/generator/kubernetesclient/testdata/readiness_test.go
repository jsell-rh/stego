package kubernetes

import (
	"encoding/json"
	"errors"
	"testing"
)

func availableDeployment(t *testing.T) Object {
	t.Helper()
	result, err := decodeObject([]byte(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"worker","namespace":"tenant","uid":"deployment-1","resourceVersion":"42","generation":9007199254740993,"labels":{"example.com/owner":"tenant-1"}},"spec":{"replicas":1},"status":{"observedGeneration":9007199254740993,"replicas":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1,"conditions":[{"type":"Available","status":"True"},{"type":"Progressing","status":"True"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDeploymentAvailability(t *testing.T) {
	owner := Owner{"example.com/owner": "tenant-1"}
	current := availableDeployment(t)
	if ready, err := DeploymentAvailable(current, owner, 1); err != nil || !ready {
		t.Fatal("available deployment was refused", err)
	}
	for name, change := range map[string]func(Object){
		"missing status":      func(o Object) { delete(o, "status") },
		"stale generation":    func(o Object) { o["status"].(map[string]any)["observedGeneration"] = json.Number("9007199254740992") },
		"future generation":   func(o Object) { o["status"].(map[string]any)["observedGeneration"] = json.Number("9007199254740994") },
		"unavailable":         func(o Object) { delete(o["status"].(map[string]any), "availableReplicas") },
		"extra old replica":   func(o Object) { o["status"].(map[string]any)["replicas"] = json.Number("2") },
		"unavailable replica": func(o Object) { o["status"].(map[string]any)["unavailableReplicas"] = json.Number("1") },
		"paused":              func(o Object) { o["spec"].(map[string]any)["paused"] = true },
		"deleting":            func(o Object) { o["metadata"].(map[string]any)["deletionTimestamp"] = "2026-09-15T00:00:00Z" },
		"different scale":     func(o Object) { o["spec"].(map[string]any)["replicas"] = json.Number("2") },
		"condition absent":    func(o Object) { delete(o["status"].(map[string]any), "conditions") },
		"condition unknown": func(o Object) {
			o["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)["status"] = "Unknown"
		},
		"rollout failed": func(o Object) {
			o["status"].(map[string]any)["conditions"].([]any)[1].(map[string]any)["status"] = "False"
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := availableDeployment(t)
			change(o)
			ready, err := DeploymentAvailable(o, owner, 1)
			if ready || err != nil {
				t.Fatal("pending deployment was not pending", ready, err)
			}
		})
	}
	for name, change := range map[string]func(Object){
		"foreign owner": func(o Object) {
			o["metadata"].(map[string]any)["labels"] = map[string]any{"example.com/owner": "other"}
		},
		"missing UID":       func(o Object) { delete(o["metadata"].(map[string]any), "uid") },
		"missing namespace": func(o Object) { delete(o["metadata"].(map[string]any), "namespace") },
		"wrong kind":        func(o Object) { o["kind"] = "ReplicaSet" },
		"string count":      func(o Object) { o["status"].(map[string]any)["availableReplicas"] = "1" },
		"float count":       func(o Object) { o["status"].(map[string]any)["availableReplicas"] = float64(1) },
		"negative count":    func(o Object) { o["status"].(map[string]any)["availableReplicas"] = json.Number("-1") },
		"overflow":          func(o Object) { o["status"].(map[string]any)["availableReplicas"] = json.Number("9223372036854775808") },
		"invalid status":    func(o Object) { o["status"] = "ready" },
		"duplicate condition": func(o Object) {
			c := o["status"].(map[string]any)["conditions"].([]any)
			o["status"].(map[string]any)["conditions"] = append(c, c[0])
		},
		"invalid condition": func(o Object) {
			o["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)["status"] = true
		},
		"too many conditions": func(o Object) { o["status"].(map[string]any)["conditions"] = make([]any, 65) },
	} {
		t.Run(name, func(t *testing.T) {
			o := availableDeployment(t)
			change(o)
			ready, err := DeploymentAvailable(o, owner, 1)
			if ready || !errors.Is(err, ErrResourceObservation) {
				t.Fatal("invalid deployment was accepted", ready, err)
			}
		})
	}
	for _, count := range []int64{-1, 0, 10001} {
		if ready, err := DeploymentAvailable(current, owner, count); ready || !errors.Is(err, ErrResourceObservation) {
			t.Fatal("invalid replica requirement was accepted")
		}
	}
}

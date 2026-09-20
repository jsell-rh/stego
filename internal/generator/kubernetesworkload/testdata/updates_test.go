package workload

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	kube "example.com/widget/kubernetes"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
)

// The fixture applies JSON Merge Patch before Kubernetes serialization.
// Object members absent from a patch remain unchanged. Arrays are replaced.
// See https://www.rfc-editor.org/rfc/rfc7396.html#section-2.
func mergeFixture(target, patch any) any {
	changes, object := patch.(map[string]any)
	if !object {
		return patch
	}
	result, object := target.(map[string]any)
	if !object {
		result = map[string]any{}
	}
	for key, value := range changes {
		if value == nil {
			delete(result, key)
			continue
		}
		result[key] = mergeFixture(result[key], value)
	}
	return result
}

func TestMergePatchFixtureRules(t *testing.T) {
	for _, tc := range []struct{ before, patch, want string }{
		{`{"a":"old","b":"keep"}`, `{"a":"new"}`, `{"a":"new","b":"keep"}`},
		{`{"a":"old","b":"keep"}`, `{"a":null}`, `{"b":"keep"}`},
		{`{"a":[1,2],"b":"keep"}`, `{"a":[]}`, `{"a":[],"b":"keep"}`},
		{`{"a":{"x":1,"y":2}}`, `{"a":{"x":null}}`, `{"a":{"y":2}}`},
	} {
		var before, patch, want any
		for raw, target := range map[string]*any{tc.before: &before, tc.patch: &patch, tc.want: &want} {
			if err := json.Unmarshal([]byte(raw), target); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(mergeFixture(before, patch), want) {
			t.Fatal("fixture merge differs")
		}
	}
}

func deploymentBytes(t *testing.T, value any) ([]byte, apps.Deployment) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var typed apps.Deployment
	if err := json.Unmarshal(raw, &typed); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(typed)
	if err != nil {
		t.Fatal(err)
	}
	return raw, typed
}

func TestWidgetDeclarationUpdatesRemoveOldValues(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Deployment)
		check  func(apps.Deployment) bool
	}{
		{"arguments", func(d *Deployment) { d.Containers[0].Args = nil }, func(d apps.Deployment) bool { return len(d.Spec.Template.Spec.Containers[0].Args) == 0 }},
		{"environment", func(d *Deployment) { d.Containers[0].Env = nil }, func(d apps.Deployment) bool { return len(d.Spec.Template.Spec.Containers[0].Env) == 0 }},
		{"empty_environment_value", func(d *Deployment) { d.Containers[0].Env[0].Value = "" }, func(d apps.Deployment) bool {
			v := d.Spec.Template.Spec.Containers[0].Env
			return len(v) == 1 && v[0].Name == "MODE" && v[0].Value == ""
		}},
		{"mounts_and_volumes", func(d *Deployment) { d.Containers[0].Mounts = nil; d.Volumes = nil }, func(d apps.Deployment) bool {
			return len(d.Spec.Template.Spec.Volumes) == 0 && len(d.Spec.Template.Spec.Containers[0].VolumeMounts) == 0
		}},
		{"pull_secrets", func(d *Deployment) { d.ImagePullSecrets = nil }, func(d apps.Deployment) bool { return len(d.Spec.Template.Spec.ImagePullSecrets) == 0 }},
		{"changed_literal", func(d *Deployment) { d.Containers[0].Env[0].Value = "updated" }, func(d apps.Deployment) bool { return d.Spec.Template.Spec.Containers[0].Env[0].Value == "updated" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := widget()
			d.Dependencies = nil
			d.Containers[0].Env = []Env{{Name: "MODE", Value: "production"}}
			d.Containers[0].Mounts = []Mount{{Name: "tmp", Path: "/tmp"}}
			d.Volumes = []Volume{{Name: "tmp", EmptyMi: 64}}
			d.ImagePullSecrets = []string{"registry-credential"}
			built := render(t, d)[0]
			_, initial := deploymentBytes(t, built.Object)
			initial.UID = "widget-uid"
			initial.ResourceVersion = "1"
			initial.Spec.Template.Spec.DNSPolicy = core.DNSClusterFirst
			initial.Spec.Template.Spec.RestartPolicy = core.RestartPolicyAlways
			current, _ := deploymentBytes(t, initial)
			var mu sync.Mutex
			writes := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Authorization") != "Bearer fixture-token" || r.URL.Path != built.Collection+"/widget" {
					t.Error("request identity or path differs")
					w.WriteHeader(http.StatusForbidden)
					return
				}
				switch r.Method {
				case http.MethodGet:
				case http.MethodPatch:
					if r.Header.Get("Content-Type") != "application/merge-patch+json" || r.URL.Query().Get("fieldValidation") != "Strict" {
						t.Error("patch type or validation differs")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					var patch, observed map[string]any
					if json.Unmarshal(raw, &patch) != nil || json.Unmarshal(current, &observed) != nil {
						t.Error("invalid JSON")
						w.WriteHeader(500)
						return
					}
					meta, ok := patch["metadata"].(map[string]any)
					if !ok || meta["uid"] != "widget-uid" || meta["resourceVersion"] != strconv.Itoa(writes+1) {
						t.Error("missing identity guard")
						w.WriteHeader(http.StatusConflict)
						return
					}
					result := mergeFixture(observed, patch)
					encoded, err := json.Marshal(result)
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					var typed apps.Deployment
					if json.Unmarshal(encoded, &typed) != nil {
						t.Error("invalid Deployment")
						w.WriteHeader(500)
						return
					}
					writes++
					typed.ResourceVersion = strconv.Itoa(writes + 1)
					current, err = json.Marshal(typed)
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
				default:
					t.Error("unexpected mutation")
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(current)
			}))
			defer server.Close()
			dir := t.TempDir()
			ca, token := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
			if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(token, []byte("fixture-token"), 0600); err != nil {
				t.Fatal(err)
			}
			client, err := kube.New(kube.Options{ServerURL: server.URL, CAFile: ca, TokenFile: token})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			owner := kube.Owner{"example.org/owner": "one"}
			if _, err := client.Ensure(ctx, built.Collection, kube.Object(built.Object), owner); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			beforeWrites := writes
			mu.Unlock()
			if beforeWrites != 0 {
				t.Fatal("API defaults caused a write")
			}
			tc.change(&d)
			wanted := render(t, d)[0]
			updated, err := client.Ensure(ctx, wanted.Collection, kube.Object(wanted.Object), owner)
			if err != nil {
				t.Fatal(err)
			}
			_, typed := deploymentBytes(t, updated)
			if !tc.check(typed) {
				t.Fatal("the previous declaration value remains")
			}
			if typed.Spec.Template.Spec.DNSPolicy != core.DNSClusterFirst || typed.Spec.Template.Spec.RestartPolicy != core.RestartPolicyAlways {
				t.Fatal("API defaults were lost")
			}
			if _, err := client.Ensure(ctx, wanted.Collection, kube.Object(wanted.Object), owner); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			afterWrites := writes
			mu.Unlock()
			if afterWrites != 1 {
				t.Fatal(fmt.Sprintf("expected one update, got %d", afterWrites))
			}
		})
	}
}

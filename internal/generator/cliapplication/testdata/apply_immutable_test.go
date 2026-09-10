package command

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func immutableApplication() Application {
	fields := []Field{{Flag: "member", Key: "member", Type: "string", Required: true}, {Flag: "group", Key: "group", Type: "string", Required: true}, {Flag: "limit", Key: "limit", Type: "integer"}}
	return Application{ConfigEnv: "TEST_CLI_CONFIG", ConfigName: "sample", Resources: []ApplyResource{{Kind: "Membership", APIVersion: "example/v1", Path: "/memberships", CreateFields: fields, ImmutableIdentity: []string{"member", "group"}}}}
}
func TestImmutableApplyDefinitionAndInput(t *testing.T) {
	for _, change := range []func(*ApplyResource){
		func(r *ApplyResource) { r.PatchFields = r.CreateFields }, func(r *ApplyResource) { r.ImmutableIdentity = []string{"member", "member"} }, func(r *ApplyResource) { r.ImmutableIdentity = []string{"missing"} }, func(r *ApplyResource) { r.ImmutableIdentity = []string{"limit"} }, func(r *ApplyResource) { r.CreateFields[0].Required = false }, func(r *ApplyResource) { r.CreateFields[0].Nullable = true },
	} {
		app := immutableApplication()
		change(&app.Resources[0])
		if validate(app) == nil {
			t.Fatal("invalid immutable definition accepted")
		}
	}
	app := immutableApplication()
	for _, spec := range []string{`{"group":"team"}`, `{"group":"team","member":null}`, `{"group":"team","member":""}`, `{"group":"team","member":"` + strings.Repeat("a", 257) + `"}`} {
		docs, err := decodeApply([]byte(manifest("Membership", "label", spec)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prepareApply(app, docs); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
	docs, err := decodeApply([]byte(manifest("Membership", "one", `{"group":"team","member":"person"}`) + "---\n" + manifest("Membership", "two", `{"group":"team","member":"person"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareApply(app, docs); err == nil {
		t.Fatal("duplicate immutable identity accepted")
	}
}

func TestImmutableApplyOverTLS(t *testing.T) {
	for _, mode := range []string{"create", "nameless", "existing", "conflict", "conflict-mismatch", "ambiguous", "wrong-row", "wrong-write", "denied"} {
		t.Run(mode, func(t *testing.T) {
			app := immutableApplication()
			directory := t.TempDir()
			t.Setenv("TEST_CLI_CONFIG", filepath.Join(directory, "config.json"))
			var calls, writes atomic.Int32
			var mu sync.Mutex
			row := map[string]any{"id": "membership-id", "member": "a' OR member = 'b", "group": "team", "limit": json.Number("9007199254740993")}
			exists := mode == "existing" || mode == "ambiguous" || mode == "wrong-row"
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("Authorization") != "Bearer immutable-token" {
					t.Error("missing credential")
				}
				if r.Method == "GET" {
					if r.URL.Query().Get("size") != "2" || r.URL.Query().Get("search") != "group = 'team' and member = 'a'' OR member = ''b'" {
						t.Error("identity query changed", r.URL.Query())
					}
					if mode == "denied" {
						w.WriteHeader(403)
						fmt.Fprint(w, `{"secret":"immutable-token"}`)
						return
					}
					if mode == "wrong-row" {
						row["member"] = "other"
					}
					items := []any{}
					if exists {
						items = append(items, row)
					}
					if mode == "ambiguous" {
						items = append(items, row)
					}
					json.NewEncoder(w).Encode(map[string]any{"items": items, "total": len(items)})
					return
				}
				writes.Add(1)
				if r.Method != "POST" {
					t.Error("immutable apply sent", r.Method)
				}
				var body map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 3 || body["name"] != nil {
					t.Error("metadata entered API body", body)
				}
				exists = true
				if strings.HasPrefix(mode, "conflict") {
					if mode == "conflict-mismatch" {
						row["limit"] = json.Number("9007199254740992")
					}
					w.WriteHeader(409)
					return
				}
				if mode == "wrong-write" {
					row["group"] = "other"
				}
				w.WriteHeader(201)
				json.NewEncoder(w).Encode(row)
			}))
			defer server.Close()
			ca, token := filepath.Join(directory, "ca.pem"), filepath.Join(directory, "token")
			os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
			os.WriteFile(token, []byte("immutable-token"), 0600)
			var output bytes.Buffer
			if err := Run(context.Background(), app, []string{"login", "--url", server.URL, "--token-file", token, "--ca-file", ca}, &output); err != nil {
				t.Fatal(err)
			}
			file := applyFile(t, filepath.Join(directory, "input.yaml"), manifest("Membership", "display-only", `{"group":"team","member":"a' OR member = 'b","limit":9007199254740993}`))
			if mode == "nameless" {
				applyFile(t, file, `{"apiVersion":"example/v1","kind":"Membership","metadata":{},"spec":{"group":"team","member":"a' OR member = 'b","limit":9007199254740993}}`)
			}
			results, rendered, err := runApplyTest(t, app, "apply", "-f", file, "-o", "json")
			want := "created"
			if mode == "existing" || mode == "conflict" {
				want = "unchanged"
			}
			success := mode == "create" || mode == "nameless" || mode == "existing" || mode == "conflict"
			if success {
				if err != nil || len(results) != 1 || results[0].Status != want || results[0].ID != "membership-id" {
					t.Fatal(results, err)
				}
				before := writes.Load()
				results, _, err = runApplyTest(t, app, "apply", "-f", file, "-o", "json")
				if err != nil || results[0].Status != "unchanged" || writes.Load() != before {
					t.Fatal("repeat wrote immutable resource", results, err)
				}
			} else if err == nil {
				t.Fatal("invalid immutable result accepted", mode)
			}
			if strings.Contains(rendered, "immutable-token") || err != nil && strings.Contains(err.Error(), "immutable-token") {
				t.Fatal("remote response leaked a token")
			}
			if (mode == "existing" || mode == "ambiguous" || mode == "wrong-row" || mode == "denied") && writes.Load() != 0 {
				t.Fatal("lookup failure caused a write", writes)
			}
			if mode == "conflict" && calls.Load() != 4 {
				t.Fatal("conflict did not resolve with one read", calls)
			}
		})
	}
}

func BenchmarkImmutableApplyComparison(b *testing.B) {
	p := applyPlan{body: map[string]json.RawMessage{}}
	row := map[string]json.RawMessage{}
	for i := 0; i < 4; i++ {
		key := fmt.Sprintf("field%d", i)
		p.resource.CreateFields = append(p.resource.CreateFields, Field{Key: key, Type: "string"})
		value, _ := json.Marshal(strings.Repeat("x", 64))
		p.body[key] = value
		row[key] = value
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := immutableMatches(&p, row); err != nil {
			b.Fatal(err)
		}
	}
}

func TestImmutableApplyFieldEquality(t *testing.T) {
	for _, tc := range []struct {
		kind, left, right string
		equal             bool
	}{
		{"integer", "9007199254740993", "9007199254740992", false},
		{"integer", "9007199254740993", "9007199254740993", true},
		{"boolean", "true", "true", true},
		{"boolean", "true", "false", false},
		{"boolean", "null", "false", false},
		{"string", "null", `""`, false},
		{"string", "null", " null ", true},
		{"string-list", `["a","b"]`, `["a","b"]`, true},
		{"string-list", `["a","b"]`, `["b","a"]`, false},
		{"string-list", "null", "[]", false},
		{"string-list", "[]", "[]", true},
	} {
		t.Run(tc.kind+tc.left+tc.right, func(t *testing.T) {
			p := applyPlan{resource: ApplyResource{CreateFields: []Field{{Key: "value", Type: tc.kind, Nullable: true}}}, body: map[string]json.RawMessage{"value": json.RawMessage(tc.left)}}
			err := immutableMatches(&p, map[string]json.RawMessage{"value": json.RawMessage(tc.right)})
			if (err == nil) != tc.equal {
				t.Fatalf("equality = %v, want %v", err == nil, tc.equal)
			}
		})
	}
}

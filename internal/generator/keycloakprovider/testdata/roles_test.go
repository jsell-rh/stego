package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type roleFixture struct {
	intercept                                               func(http.ResponseWriter, *http.Request) bool
	hiddenScopes, denyScopeProbe                            bool
	mu                                                      sync.Mutex
	clients                                                 map[string]ClientRepresentation
	roles                                                   map[string]RoleRepresentation
	current                                                 map[string]roleSet
	groups                                                  map[string][]string
	groupRoles                                              map[string][]RoleRepresentation
	enabled                                                 map[string]bool
	serviceSubject                                          string
	ignoreDelete, ignoreAdd, failAdd, inherited, malformed  bool
	mappers                                                 []protocolMapper
	mapperSerial                                            int
	malformedMappers, scopeOnMapperDelete                   bool
	writes                                                  []string
	scopes                                                  map[string][]assignedScope
	ignoreScopeDelete, malformedScopes, changeOwnerOnDelete bool
}

func testRole(id, name, client string) RoleRepresentation {
	container := client
	if container == "" {
		container = "realm-id"
	}
	return RoleRepresentation{ID: id, Name: name, ContainerID: container, ClientRole: client != ""}
}
func roleBindings() (ClientBinding, ClientBinding) {
	return ClientBinding{ID: "worker", ClientID: "batch-worker", Attributes: map[string]string{"pipeline.job": "run-1"}}, ClientBinding{ID: "target", ClientID: "catalog", Attributes: map[string]string{"product.owner": "object-1"}}
}
func newRoleFixture(t *testing.T) (*Client, *roleFixture) {
	t.Helper()
	owner, target := roleBindings()
	f := &roleFixture{clients: map[string]ClientRepresentation{}, roles: map[string]RoleRepresentation{}, current: map[string]roleSet{}, groups: map[string][]string{}, groupRoles: map[string][]RoleRepresentation{}, enabled: map[string]bool{"human": true, "service-user": true}, serviceSubject: "service-user"}
	for _, b := range []ClientBinding{owner, target, {ID: "foreign", ClientID: "foreign-app", Attributes: map[string]string{"another.owner": "other"}}} {
		f.clients[b.ID] = ClientRepresentation{ID: b.ID, ClientID: b.ClientID, Protocol: "openid-connect", ServiceAccountsEnabled: true, Attributes: b.Attributes}
	}
	for _, r := range []RoleRepresentation{testRole("read-id", "read", "target"), testRole("write-id", "write", "target"), testRole("old-id", "old", "target"), testRole("foreign-id", "other", "foreign"), testRole("realm-old", "old-realm", ""), testRole("realm-base", "base", "")} {
		f.roles[r.ContainerID+"/"+r.Name] = r
	}
	for _, subject := range []string{"human", "service-user"} {
		f.current[subject] = roleSet{Realm: []RoleRepresentation{f.roles["realm-id/old-realm"]}, Clients: map[string][]RoleRepresentation{"target": {f.roles["target/old"]}, "foreign": {f.roles["foreign/other"]}}}
		f.groups[subject] = []string{"outside-group"}
	}
	f.scopes = map[string][]assignedScope{
		"default-client-scopes":  {{ID: "default-roles", Name: "roles", Protocol: "openid-connect"}},
		"optional-client-scopes": {{ID: "optional-email", Name: "email", Protocol: "openid-connect"}},
	}
	f.current["scope:worker"] = roleSet{Realm: []RoleRepresentation{f.roles["realm-id/old-realm"]}, Clients: map[string][]RoleRepresentation{"foreign": {f.roles["foreign/other"]}, "target": {f.roles["target/old"]}}}
	f.enabled["scope:worker"] = true
	f.groupRoles["outside-group"] = []RoleRepresentation{f.roles["foreign/other"]}
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if authRequest(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.intercept != nil && f.intercept(w, r) {
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/admin/realms/tenant/")
		parts := strings.Split(path, "/")
		send := func(value any) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(value); err != nil {
				t.Error(err)
			}
		}
		if path == "client-scopes" && r.Method == "GET" {
			if f.hiddenScopes {
				send([]assignedScope{})
			} else {
				send([]assignedScope{{ID: "scope-proof", Name: "scope-proof", Protocol: "openid-connect"}})
			}
			return
		}
		if path == "client-scopes/scope-proof" && r.Method == "GET" {
			if f.denyScopeProbe {
				w.WriteHeader(403)
			} else {
				send(assignedScope{ID: "scope-proof", Name: "scope-proof", Protocol: "openid-connect"})
			}
			return
		}
		if len(parts) >= 3 && parts[0] == "clients" && parts[2] == "scope-mappings" {
			// Reuse the role-set wire fixture for the distinct scope endpoints.
			parts = append([]string{"users", "scope:" + parts[1], "role-mappings"}, parts[3:]...)
		}
		if len(parts) >= 2 && parts[0] == "clients" {
			client, ok := f.clients[parts[1]]
			if !ok {
				w.WriteHeader(404)
				return
			}
			if len(parts) >= 4 && parts[2] == "protocol-mappers" && parts[3] == "models" {
				if parts[1] != "worker" {
					t.Error("unexpected mapper owner")
					w.WriteHeader(500)
					return
				}
				if len(parts) == 4 && r.Method == "GET" {
					if f.malformedMappers {
						send(nil)
					} else {
						send(f.mappers)
					}
					return
				}
				if len(parts) == 5 && r.Method == "DELETE" {
					f.writes = append(f.writes, "DELETE "+path)
					if !f.ignoreDelete {
						keep := []protocolMapper{}
						for _, m := range f.mappers {
							if m.ID != parts[4] {
								keep = append(keep, m)
							}
						}
						f.mappers = keep
					}
					if f.scopeOnMapperDelete {
						f.scopes["default-client-scopes"] = []assignedScope{{ID: "injected", Name: "injected", Protocol: "openid-connect"}}
					}
					w.WriteHeader(204)
					return
				}
				if len(parts) == 4 && r.Method == "POST" {
					f.writes = append(f.writes, "POST "+path)
					var m protocolMapper
					if json.NewDecoder(r.Body).Decode(&m) != nil || m.ID != "" {
						t.Error("invalid mapper creation")
						w.WriteHeader(400)
						return
					}
					if f.failAdd {
						w.WriteHeader(503)
						return
					}
					if !f.ignoreAdd {
						f.mapperSerial++
						m.ID = fmt.Sprintf("mapper-%d", f.mapperSerial)
						f.mappers = append(f.mappers, m)
					}
					w.WriteHeader(201)
					return
				}
				t.Error("unexpected mapper request")
				w.WriteHeader(500)
				return
			}
			if len(parts) >= 3 && (parts[2] == "default-client-scopes" || parts[2] == "optional-client-scopes") {
				if parts[1] != "worker" {
					t.Error("unexpected scope owner")
					w.WriteHeader(500)
					return
				}
				if len(parts) == 3 && r.Method == "GET" {
					if f.malformedScopes {
						send(nil)
					} else {
						// The real assignment endpoints return only identity fields.
						values := []map[string]string{}
						if !f.hiddenScopes {
							for _, scope := range f.scopes[parts[2]] {
								values = append(values, map[string]string{"id": scope.ID, "name": scope.Name})
							}
						}
						send(values)
					}
					return
				}
				if len(parts) == 4 && r.Method == "DELETE" {
					f.writes = append(f.writes, "DELETE "+path)
					if !f.ignoreScopeDelete {
						keep := []assignedScope{}
						for _, scope := range f.scopes[parts[2]] {
							if scope.ID != parts[3] {
								keep = append(keep, scope)
							}
						}
						f.scopes[parts[2]] = keep
					}
					if f.changeOwnerOnDelete {
						client.Attributes = map[string]string{"pipeline.job": "other"}
						f.clients[parts[1]] = client
					}
					w.WriteHeader(204)
					return
				}
				t.Error("unexpected scope assignment request")
				w.WriteHeader(500)
				return
			}
			if len(parts) == 2 && r.Method == "GET" {
				send(client)
				return
			}
			if len(parts) == 3 && parts[2] == "service-account-user" && r.Method == "GET" {
				send(map[string]any{"id": f.serviceSubject, "enabled": true})
				return
			}
			if len(parts) == 4 && parts[2] == "roles" && r.Method == "GET" {
				role, ok := f.roles[parts[1]+"/"+parts[3]]
				if !ok {
					w.WriteHeader(404)
				} else {
					send(role)
				}
				return
			}
			if len(parts) == 3 && parts[2] == "roles" && r.Method == "POST" {
				f.writes = append(f.writes, "POST "+path)
				var role RoleRepresentation
				if json.NewDecoder(r.Body).Decode(&role) != nil || role.Name == "" || role.Composite {
					t.Error("invalid role creation")
					w.WriteHeader(400)
					return
				}
				key := parts[1] + "/" + role.Name
				if _, ok := f.roles[key]; ok {
					w.WriteHeader(409)
					return
				}
				f.roles[key] = testRole("created-"+role.Name, role.Name, parts[1])
				w.WriteHeader(201)
				return
			}
		}
		if len(parts) == 2 && parts[0] == "roles" && r.Method == "GET" {
			role, ok := f.roles["realm-id/"+parts[1]]
			if !ok {
				w.WriteHeader(404)
			} else {
				send(role)
			}
			return
		}
		if len(parts) >= 2 && parts[0] == "users" {
			subject := parts[1]
			enabled, exists := f.enabled[subject]
			if !exists {
				w.WriteHeader(404)
				return
			}
			if len(parts) == 2 && r.Method == "GET" {
				send(map[string]any{"id": subject, "enabled": enabled})
				return
			}
			if len(parts) == 3 && parts[2] == "groups" && r.Method == "GET" {
				if r.URL.Query().Get("max") != "65" || r.URL.Query().Get("first") != "0" || r.URL.Query().Get("briefRepresentation") != "true" {
					t.Error("unbounded group read")
				}
				groups := []map[string]string{}
				for _, id := range f.groups[subject] {
					groups = append(groups, map[string]string{"id": id})
				}
				send(groups)
				return
			}
			if len(parts) == 4 && parts[2] == "groups" && r.Method == "DELETE" {
				f.writes = append(f.writes, "DELETE "+path)
				if !f.ignoreDelete {
					var keep []string
					for _, id := range f.groups[subject] {
						if id != parts[3] {
							keep = append(keep, id)
						}
					}
					f.groups[subject] = keep
				}
				w.WriteHeader(204)
				return
			}
			if len(parts) >= 3 && parts[2] == "role-mappings" {
				current := f.current[subject]
				if len(parts) == 3 && r.Method == "GET" {
					if f.malformed {
						send(map[string]any{"clientMappings": map[string]any{"catalog": map[string]any{"id": "../foreign", "client": "catalog", "mappings": []any{}}}})
						return
					}
					clients := map[string]any{}
					for id, roles := range current.Clients {
						if len(roles) > 0 {
							clients[f.clients[id].ClientID] = map[string]any{"id": id, "client": f.clients[id].ClientID, "mappings": roles}
						}
					}
					send(map[string]any{"realmMappings": current.Realm, "clientMappings": clients})
					return
				}
				clientID := ""
				if len(parts) >= 5 && parts[3] == "clients" {
					clientID = parts[4]
				} else if len(parts) < 4 || parts[3] != "realm" {
					t.Error("unexpected mapping path", path)
					w.WriteHeader(500)
					return
				}
				roles := current.Realm
				if clientID != "" {
					roles = current.Clients[clientID]
				}
				if r.Method == "GET" {
					result := append([]RoleRepresentation{}, roles...)
					if parts[len(parts)-1] == "composite" {
						for _, group := range f.groups[subject] {
							for _, role := range f.groupRoles[group] {
								if role.ClientRole == (clientID != "") && (clientID == "" || role.ContainerID == clientID) {
									present := false
									for _, r := range result {
										if r.ID == role.ID {
											present = true
										}
									}
									if !present {
										result = append(result, role)
									}
								}
							}
						}
						if f.inherited && clientID == "target" {
							result = append(result, testRole("hidden", "inherited", "target"))
						}
					}
					send(result)
					return
				}
				if r.Method == "POST" || r.Method == "DELETE" {
					f.writes = append(f.writes, r.Method+" "+path)
					if r.Method == "POST" && f.failAdd {
						w.WriteHeader(503)
						return
					}
					var assigned []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					}
					decoder := json.NewDecoder(r.Body)
					decoder.DisallowUnknownFields()
					if decoder.Decode(&assigned) != nil || len(assigned) == 0 {
						t.Error("invalid assignment body")
						w.WriteHeader(400)
						return
					}
					if !(r.Method == "DELETE" && f.ignoreDelete) && !(r.Method == "POST" && f.ignoreAdd) {
						for _, change := range assigned {
							if r.Method == "DELETE" {
								var keep []RoleRepresentation
								for _, role := range roles {
									if role.ID != change.ID {
										keep = append(keep, role)
									}
								}
								roles = keep
							} else {
								container := clientID
								if container == "" {
									container = "realm-id"
								}
								role, ok := f.roles[container+"/"+change.Name]
								if !ok || role.ID != change.ID {
									t.Error("assignment identity differs")
									w.WriteHeader(400)
									return
								}
								roles = append(roles, role)
							}
						}
						if clientID == "" {
							current.Realm = roles
						} else {
							current.Clients[clientID] = roles
						}
						f.current[subject] = current
					}
					w.WriteHeader(204)
					return
				}
			}
		}
		t.Error("unexpected role request", r.Method, path)
		w.WriteHeader(500)
	})
	return c, f
}
func TestSharedUserRolesPreserveOtherAccess(t *testing.T) {
	c, f := newRoleFixture(t)
	_, target := roleBindings()
	ctx := context.Background()
	if err := c.ReconcileUserClientRoles(ctx, target, "human", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	state := f.current["human"]
	writes := append([]string{}, f.writes...)
	groups := len(f.groups["human"])
	f.mu.Unlock()
	if len(state.Clients["target"]) != 1 || state.Clients["target"][0].ID != "read-id" || len(state.Clients["foreign"]) != 1 || len(state.Realm) != 1 || groups != 1 {
		t.Fatal("scoped roles changed unrelated access")
	}
	if len(writes) != 2 || !strings.HasPrefix(writes[0], "DELETE ") || !strings.HasPrefix(writes[1], "POST ") {
		t.Fatal("removal did not precede addition", writes)
	}
	if err := c.ReconcileUserClientRoles(ctx, target, "human", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) != 2 {
		t.Fatal("converged user roles caused a write")
	}
}
func TestUserRoleFailureDoesNotAddAccess(t *testing.T) {
	for _, failure := range []string{"ignored-delete", "inherited", "foreign-owner", "disabled-user", "composite-role"} {
		t.Run(failure, func(t *testing.T) {
			c, f := newRoleFixture(t)
			_, target := roleBindings()
			switch failure {
			case "ignored-delete":
				f.ignoreDelete = true
			case "inherited":
				f.inherited = true
			case "foreign-owner":
				target.Attributes = map[string]string{"product.owner": "another"}
			case "disabled-user":
				f.enabled["human"] = false
			case "composite-role":
				r := f.roles["target/read"]
				r.Composite = true
				f.roles["target/read"] = r
			}
			if err := c.ReconcileUserClientRoles(context.Background(), target, "human", []string{"read"}); err == nil {
				t.Fatal("unsafe role update accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, write := range f.writes {
				if strings.HasPrefix(write, "POST ") {
					t.Fatal("access added after failed removal or ownership", write)
				}
			}
		})
	}
}
func TestUserRolePartialFailureRecovers(t *testing.T) {
	c, f := newRoleFixture(t)
	_, target := roleBindings()
	f.failAdd = true
	if err := c.ReconcileUserClientRoles(context.Background(), target, "human", []string{"read"}); err == nil {
		t.Fatal("failed addition succeeded")
	}
	f.mu.Lock()
	if len(f.writes) != 2 {
		t.Fatal("failed mutation was replayed")
	}
	f.failAdd = false
	f.mu.Unlock()
	if err := c.ReconcileUserClientRoles(context.Background(), target, "human", []string{"read"}); err != nil {
		t.Fatal("partial removal did not recover", err)
	}
}
func TestOwnedServiceAccountRolesRemoveEveryOtherSource(t *testing.T) {
	c, f := newRoleFixture(t)
	owner, target := roleBindings()
	p := ServiceAccountRolePolicy{Realm: []string{"base"}, Clients: []ClientRoleGrant{{Client: target, Names: []string{"read"}}}}
	if err := c.ReconcileServiceAccountRoles(context.Background(), owner, "service-user", p); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	state := f.current["service-user"]
	groups := len(f.groups["service-user"])
	writes := append([]string{}, f.writes...)
	human := f.current["human"]
	f.mu.Unlock()
	if len(state.Realm) != 1 || state.Realm[0].ID != "realm-base" || len(state.Clients["target"]) != 1 || state.Clients["target"][0].ID != "read-id" || len(state.Clients["foreign"]) != 0 || groups != 0 {
		t.Fatal("owned role policy did not replace all access")
	}
	if len(human.Realm) != 1 || human.Realm[0].ID != "realm-old" || len(human.Clients["foreign"]) != 1 {
		t.Fatal("owned reconciliation changed another user")
	}
	added := false
	for _, write := range writes {
		if strings.HasPrefix(write, "POST ") {
			added = true
		} else if added {
			t.Fatal("removal happened after addition", writes)
		}
	}
	if err := c.ReconcileServiceAccountRoles(context.Background(), owner, "service-user", p); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) != len(writes) {
		t.Fatal("converged owned roles caused a write")
	}
}
func TestOwnedRoleBoundaryAndConfirmation(t *testing.T) {
	for _, failure := range []string{"human-subject", "changed-subject", "enabled-client", "foreign-target", "ignored-delete", "inherited", "ignored-add", "malformed"} {
		t.Run(failure, func(t *testing.T) {
			c, f := newRoleFixture(t)
			owner, target := roleBindings()
			subject := "service-user"
			switch failure {
			case "human-subject":
				subject = "human"
			case "changed-subject":
				f.serviceSubject = "replacement-user"
			case "enabled-client":
				r := f.clients["worker"]
				r.Enabled = true
				f.clients["worker"] = r
			case "foreign-target":
				target.Attributes = map[string]string{"product.owner": "another"}
			case "ignored-delete":
				f.ignoreDelete = true
			case "inherited":
				f.inherited = true
			case "ignored-add":
				f.ignoreAdd = true
			case "malformed":
				f.malformed = true
			}
			p := ServiceAccountRolePolicy{Clients: []ClientRoleGrant{{Client: target, Names: []string{"read"}}}}
			if err := c.ReconcileServiceAccountRoles(context.Background(), owner, subject, p); err == nil {
				t.Fatal("unconfirmed or unsafe owned roles accepted")
			}
			if failure != "ignored-add" {
				f.mu.Lock()
				defer f.mu.Unlock()
				for _, write := range f.writes {
					if strings.HasPrefix(write, "POST ") {
						t.Fatal("new grant followed failed boundary or removal")
					}
				}
			}
		})
	}
}
func TestEnsureClientRoles(t *testing.T) {
	c, f := newRoleFixture(t)
	_, target := roleBindings()
	got, err := c.EnsureClientRoles(context.Background(), target, []string{"read", "new-role"})
	if err != nil || len(got) != 2 {
		t.Fatal("role creation failed", err)
	}
	if _, err = c.EnsureClientRoles(context.Background(), target, []string{"read", "new-role"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) != 1 {
		t.Fatal("existing roles were rewritten")
	}
}
func TestRoleInputAndResponseValidation(t *testing.T) {
	for _, names := range [][]string{{"../admin"}, {"read", "read"}, {"a/b"}, {"a%2fb"}, {""}} {
		if validateRoleNames(names) == nil {
			t.Fatal("unsafe role names accepted")
		}
	}
	for _, body := range []string{`null`, `[{"id":"r","name":"read","containerId":"target","clientRole":true}]`, `[{"id":"r","name":"read","containerId":"other","clientRole":true,"composite":false}]`, `[{"id":"../r","name":"read","containerId":"target","clientRole":true,"composite":false}]`} {
		if _, err := parseRoles([]byte(body), "target"); !errors.Is(err, ErrResponse) {
			t.Fatal("invalid role response accepted", err)
		}
	}
	c, _ := newRoleFixture(t)
	_, target := roleBindings()
	if err := c.ReconcileUserClientRoles(context.Background(), target, "missing-user", nil); err != nil {
		t.Fatal("absent user revocation failed", err)
	}
	if err := c.ReconcileUserClientRoles(context.Background(), target, "missing-user", []string{"read"}); !errors.Is(err, ErrNotFound) {
		t.Fatal("grant to missing user accepted", err)
	}
}

func TestScopedRoleSubjectCanUseFederatedIdentitySyntax(t *testing.T) {
	c, f := newRoleFixture(t)
	_, target := roleBindings()
	subject := "f:storage:person@example.com"
	f.enabled[subject] = true
	f.current[subject] = roleSet{Clients: map[string][]RoleRepresentation{}}
	if err := c.ReconcileUserClientRoles(context.Background(), target, subject, []string{"read"}); err != nil {
		t.Fatal("federated subject syntax was rejected", err)
	}
	for _, subject := range []string{"../human", "human/other", "human%2fother", "human?user=other", "human#other"} {
		if err := c.ReconcileUserClientRoles(context.Background(), target, subject, []string{"read"}); err == nil {
			t.Fatal("unsafe subject accepted")
		}
	}
}

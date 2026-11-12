package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/audit/auditoutbox"
	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/command"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/internal/staging"
	"github.com/jsell-rh/stego/registry/runtime/manifest"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/query"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// tenantDecision is a decided Provision request, the one source of a tenant that a test may build.
type tenantDecision struct{ seal.DecisionMark }

func mustTenant(t *testing.T, id string) tenant.Tenant {
	t.Helper()
	tn, err := tenant.New(tenant.Requested(tenant.ProvisionAction, id, tenantDecision{DecisionMark: seal.NewDecisionMark(id, "provision")}))
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

func mustPrincipal(t *testing.T, subject string) authz.Principal {
	t.Helper()
	p, err := authz.StagingPrincipal(staging.NewStaging(), subject)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type order struct{ ID, Status string }

const orderColumns = "id, status, version, authz_token"

// orderEntity is the command entity of the orders table, as the generated code declares it.
func orderEntity(t *testing.T) *command.Entity[order] {
	t.Helper()
	selectRow := "SELECT " + orderColumns + " FROM public.orders WHERE tenant_id = $1 AND id = $2"
	e, err := command.NewEntity(command.EntitySpec[order]{
		Name: "Order", ResourceType: "order", ReadPermission: "view", ListPermission: "view",
		Select: selectRow, SelectForUpdate: selectRow + " FOR NO KEY UPDATE", SelectForDelete: selectRow + " FOR UPDATE",
		Insert: "INSERT INTO public.orders (tenant_id, id, status) VALUES ($1, $2, $3) RETURNING " + orderColumns,
		Update: "UPDATE public.orders SET status = $5, version = version + 1 WHERE tenant_id = $1 AND id = $2 AND version = $3 AND $4::text = id RETURNING " + orderColumns,
		Delete: "DELETE FROM public.orders WHERE tenant_id = $1 AND id = $2 AND version = $3",
		Scan: func(s command.Scanner) (order, command.Meta, error) {
			var v order
			var m command.Meta
			err := s.Scan(&v.ID, &v.Status, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		Values:     func(v order) []any { return []any{v.ID, v.Status} },
		State:      func(v order) map[string]any { return map[string]any{"id": v.ID, "status": v.Status} },
		KeyChanged: func(order, order) bool { return false },
		Clone:      func(v order) order { return v },
		ValidID:    func(id string) bool { return id != "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// parts are the declared parts of the test service.
func parts(entities []command.Declared, calls *int) Parts {
	return Parts{
		Service: "todo-svc", DeclarationVersion: "d1", SubjectType: "user", TenantClaim: "tenant", ProblemCode: "TODO",
		TenantConfig: tenantConfig(calls), OwnerRole: fixtureOwner,
		NewQueue: func(db *sql.DB) (*outbox.Queue, error) {
			return outbox.New(db, outbox.Options{PayloadLimits: auditoutbox.PayloadLimits()})
		},
		AuditQueue: func(q *outbox.Queue) (audit.Enqueuer, error) { return auditoutbox.NewQueue(q) },
		NewAuthz: func(db *sql.DB, provider authz.Decision, q *outbox.Queue) (*authz.Runtime, error) {
			return authz.New(authz.Config{Provider: provider, DB: db, Schema: "public", Outbox: q})
		},
		AuditSettings: audit.Settings{Level: audit.LevelChange},
		Commands:      entities,
		// The anchor period of the audit DDL of the migration job of the fixture.
		Workers: WorkerParts{AnchorPeriod: time.Hour},
	}
}

// relate writes a relationship of the tenant t1 to SpiceDB: the subject holds relation on the resource key.
func relate(t *testing.T, endpoint, key, resourceType, resourceKey, relation, subject string) {
	t.Helper()
	conn, err := DialSpiceDB(SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	id, err := authz.ResourceID(mustTenant(t, "t1"), resourceKey)
	if err != nil {
		t.Fatal(err)
	}
	subjectType, subjectID, _ := strings.Cut(subject, ":")
	if _, err := v1.NewPermissionsServiceClient(conn).WriteRelationships(context.Background(), &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{Operation: v1.RelationshipUpdate_OPERATION_TOUCH, Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: id}, Relation: relation,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: subjectType, ObjectId: subjectID}}}}}}); err != nil {
		t.Fatal(err)
	}
}

func TestNew(t *testing.T) {
	t.Run("ST-SUP-RP14-LC02-T01 New wires the runtimes of a service from its settings", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		spice := SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}
		if err := Migrate(context.Background(), MigrationSettings{DatabaseURL: f.database, SpiceDB: spice}, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		relate(t, endpoint, key, "service", "todo-svc", "operator", "user:u-7")
		relate(t, endpoint, key, "order", "o1", "owner", "user:u-7")
		if _, err := f.admin.Exec(`INSERT INTO public.orders (tenant_id, id, status) VALUES ('t1', 'o1', 'open')`); err != nil {
			t.Fatal(err)
		}
		signing := authntest.NewKey(t, "k1")
		settings := Settings{DatabaseURL: f.runtimeURL(), Issuer: "https://issuer.example", Audience: "todo",
			KeysFile: writeFile(t, "keys.json", authntest.Document(signing.JWK())), SpiceDB: spice,
			CursorKey: []byte(strings.Repeat("k", 32)), ServiceVersion: "1.0.0"}
		orders := orderEntity(t)
		calls := 0
		svc, err := New(context.Background(), settings, parts([]command.Declared{orders}, &calls))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = svc.Close() })
		if !tenant.Opened(svc.pool) || !tenant.Opened(svc.auditDB) || svc.pool == svc.auditDB || calls != 1 {
			t.Fatalf("New opened its pools %v %v from %d adapter configurations, want two pools of tenant.Open from one", tenant.Opened(svc.pool), tenant.Opened(svc.auditDB), calls)
		}
		if svc.Verifier() == nil || svc.Commands() == nil || svc.Collections() != nil || svc.Queries() == nil || svc.Probes() == nil ||
			svc.Authz() == nil || svc.Writer() == nil || svc.Queue() == nil || svc.Telemetry() == nil {
			t.Fatal("New did not give each runtime")
		}

		// One command through the command runtime: decided on the service-level resource, written in the tenant.
		cmd, err := command.NewCommand(command.CommandSpec[struct{}]{Name: "create-order", Writes: []command.Declared{orders},
			Probe: func(*probe.Probe) struct{} { return struct{}{} }, Permission: "create_order",
			Resource: &command.Resource{Type: "service", Service: "todo-svc"}})
		if err != nil {
			t.Fatal(err)
		}
		create := func(subject, id string) error {
			fingerprint, err := idempotency.Fingerprint("POST", "/orders", []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			_, err = command.Run(context.Background(), svc.Commands(), cmd, command.Request{Tenant: mustTenant(t, "t1"),
				Principal: mustPrincipal(t, subject), Key: "k-" + id, Fingerprint: fingerprint, Body: []byte(`{}`)},
				func([]byte) (struct{}, error) { return struct{}{}, nil },
				func(ctx context.Context, _ struct{}, h *command.Handle[struct{}]) (idempotency.Response, error) {
					repo, err := command.Repo(h, orders)
					if err != nil {
						return idempotency.Response{}, err
					}
					_, err = repo.Create(ctx, order{ID: id, Status: "new"}, nil)
					return idempotency.Response{Status: 201, Body: []byte(`{}`)}, err
				})
			return err
		}
		if err := create("user:u-7", "o2"); err != nil {
			t.Fatalf("the command gave %v", err)
		}
		var tenantOf string
		if err := f.admin.QueryRow(`SELECT tenant_id FROM public.orders WHERE id = 'o2'`).Scan(&tenantOf); err != nil || tenantOf != "t1" {
			t.Fatalf("the command wrote its row in the tenant %q: %v", tenantOf, err)
		}
		if err := create("user:u-9", "o3"); !errors.Is(err, command.ErrDenied) {
			t.Fatalf("the command of a user with no permission gave %v, want ErrDenied", err)
		}

		// One query through the query runtime: decided at SpiceDB in the tenant.
		entity, err := query.NewEntity(query.Spec{Name: "order", Table: "orders", Key: "id", Columns: []string{"id", "status"},
			ResourceType: "order", ReadPermission: "view", ListPermission: "view"},
			func(r query.Row) (order, error) {
				id, err := r.Text("id")
				if err != nil {
					return order{}, err
				}
				status, err := r.Text("status")
				return order{ID: id, Status: status}, err
			})
		if err != nil {
			t.Fatal(err)
		}
		q, err := query.NewQuery("get-order", entity)
		if err != nil {
			t.Fatal(err)
		}
		get := func(subject, id string) (order, error) {
			tn, p := mustTenant(t, "t1"), mustPrincipal(t, subject)
			b := query.Bound[struct{}]{Tenant: tn, Principal: p, Call: svc.Probes().Bind(context.Background(), tn.ID(), p.Subject().String(), q.Name()),
				Probe: func(*probe.Probe) struct{} { return struct{}{} }}
			return query.Run(context.Background(), svc.Queries(), q, b, id, func(ctx context.Context, id string, h *query.Handle[struct{}]) (order, error) {
				repo, err := query.Repo(h, entity)
				if err != nil {
					return order{}, err
				}
				o, err := repo.Get(ctx, id)
				if err != nil {
					return order{}, err
				}
				return o.Value(), nil
			})
		}
		if o, err := get("user:u-7", "o1"); err != nil || o.Status != "open" {
			t.Fatalf("the query gave %+v, %v", o, err)
		}
		if _, err := get("user:u-9", "o1"); !errors.Is(err, query.ErrNotFound) {
			t.Fatalf("the query of a user with no permission gave %v, want not found", err)
		}

		// New is refused when SpiceDB cannot be reached, and when a part is missing.
		unreachable := settings
		unreachable.SpiceDB.Endpoint = "127.0.0.1:1"
		if _, err := New(context.Background(), unreachable, parts([]command.Declared{orderEntity(t)}, nil)); err == nil || !strings.Contains(err.Error(), "SpiceDB") {
			t.Fatalf("New with an unreachable SpiceDB gave %v", err)
		}
		missing := parts(nil, nil)
		missing.TenantConfig = nil
		if _, err := New(context.Background(), settings, missing); err == nil {
			t.Fatal("New took parts with no connection configuration")
		}

		t.Run("ST-SUP-RP14-LC05-T01 The verifier of a declared issuer comes from its discovery document, and a refused start names its stage", func(t *testing.T) {
			provider := authntest.NewServer(t)
			es := authntest.NewECKey(t, "e1")
			provider.Publish(es)
			issuer := provider.Issuer("/realms/todo")
			oidc := settings
			oidc.KeysFile, oidc.Issuer, oidc.Discovery, oidc.CAFile = "", issuer, true, provider.CAFile(t)
			oidc.Algorithms, oidc.SubjectClaim = []string{authn.AlgorithmES256}, "oid"
			svc, err := New(context.Background(), oidc, parts(nil, nil))
			if err != nil {
				t.Fatalf("New with the discovery of the issuer gave %v", err)
			}
			defer func() { _ = svc.Close() }()
			claims := authntest.Claims(issuer, "todo", "user-1", time.Now())
			claims["oid"], claims["tenant"] = "object-7", "t1"
			id, err := svc.Verifier().Verify(context.Background(), authntest.Mint(t, authntest.HeaderOf("ES256", "e1"), claims, es))
			if err != nil || id.UserID() != "object-7" || provider.Discoveries() != 1 {
				t.Fatalf("the verifier gave %q, %v after %d discovery requests, want object-7 after one", id.UserID(), err, provider.Discoveries())
			}
			_, refused := svc.Verifier().Verify(context.Background(), authntest.Mint(t, authntest.Header("e1"), claims, authntest.NewKey(t, "e1")))
			if refused == nil {
				t.Fatal("the verifier with the declared algorithm ES256 accepted an RS256 header")
			}
			if StartStage(refused) != "" || StartStage(errors.New("other")) != "" {
				t.Fatal("StartStage named a stage for a failure that is not of the key source")
			}

			provider.SetDiscovery(authntest.Discovery("https://other.authn.test/realms/todo", provider.URL()))
			_, err = New(context.Background(), oidc, parts(nil, nil))
			if stage := StartStage(err); err == nil || stage != "service.start.authn.discovery" {
				t.Fatalf("New with a discovery document of another issuer gave %v at the stage %q", err, stage)
			}
			keyURL := settings
			keyURL.KeysFile, keyURL.KeysURL = "", provider.URL()
			_, err = New(context.Background(), keyURL, parts(nil, nil))
			if stage := StartStage(err); err == nil || stage != "service.start.authn.keys" {
				t.Fatalf("New with a key URL that the system roots do not trust gave %v at the stage %q", err, stage)
			}
		})

		t.Run("ST-SUP-RP14-LC06-T01 New refuses a database that does not hold the schema manifest before it opens anything else", func(t *testing.T) {
			orders := manifest.Table{Schema: "public", Name: "orders", RowSecurity: true,
				Columns: []manifest.Column{{Name: "tenant_id", Type: "text", NotNull: true}, {Name: "status", Type: "text", NotNull: true}},
				Policies: []manifest.Policy{{Name: "orders_tenant", Command: "*", Using: "(tenant_id = current_setting('app.tenant'::text, true))",
					WithCheck: "(tenant_id = current_setting('app.tenant'::text, true))"}}}
			declared := parts(nil, nil)
			declared.Schema = manifest.Manifest{Tables: []manifest.Table{orders}}
			// The database of the migration job holds the manifest, so New starts.
			svc, err := New(context.Background(), settings, declared)
			if err != nil {
				t.Fatalf("New refused the database that the migration job made: %v", err)
			}
			_ = svc.Close()
			refused := func(change, restore, fault string) {
				t.Helper()
				if _, err := f.admin.Exec(change); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := f.admin.Exec(restore); err != nil {
						t.Fatal(err)
					}
				}()
				var outbox, audit int64
				if err := f.admin.QueryRow(`SELECT (SELECT count(*) FROM stego_outbox.messages), (SELECT count(*) FROM public.audit_event)`).Scan(&outbox, &audit); err != nil {
					t.Fatal(err)
				}
				// SpiceDB cannot be reached: a refusal that names the manifest comes before New connects to it.
				_, err := New(context.Background(), unreachable, declared)
				if !errors.Is(err, manifest.ErrNotSatisfied) || !strings.Contains(err.Error(), fault) || strings.Contains(err.Error(), "SpiceDB") {
					t.Fatalf("New after %q gave %v, want a refusal that names %q", change, err, fault)
				}
				var outboxAfter, auditAfter int64
				if err := f.admin.QueryRow(`SELECT (SELECT count(*) FROM stego_outbox.messages), (SELECT count(*) FROM public.audit_event)`).Scan(&outboxAfter, &auditAfter); err != nil {
					t.Fatal(err)
				}
				if outboxAfter != outbox || auditAfter != audit {
					t.Fatalf("the refused start wrote: outbox %d then %d, audit %d then %d", outbox, outboxAfter, audit, auditAfter)
				}
			}
			refused("ALTER TABLE public.orders ALTER COLUMN status TYPE character varying(20)", "ALTER TABLE public.orders ALTER COLUMN status TYPE text",
				"table public.orders: column status has the type character varying(20)")
			refused("ALTER TABLE public.orders NO FORCE ROW LEVEL SECURITY", "ALTER TABLE public.orders FORCE ROW LEVEL SECURITY",
				"table public.orders: row-level security is not enabled and forced")
			// Each table of the manifest belongs to the owner role of the service.
			refused("ALTER TABLE public.orders OWNER TO "+f.relay, "ALTER TABLE public.orders OWNER TO "+f.owner,
				"table public.orders: the table belongs to the role "+f.relay+", and the manifest needs "+f.owner)
			// The audit record is in the manifest of every service, with the triggers that make it append-only.
			refused("DROP TRIGGER audit_event_reject_row ON public.audit_event",
				"CREATE TRIGGER audit_event_reject_row BEFORE UPDATE OR DELETE ON public.audit_event FOR EACH ROW EXECUTE FUNCTION public.audit_reject_change()",
				"table public.audit_event: trigger audit_event_reject_row is missing")
			if svc, err := New(context.Background(), settings, declared); err != nil {
				t.Fatalf("New refused the restored database: %v", err)
			} else {
				_ = svc.Close()
			}
		})
	})
}

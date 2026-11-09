package authz

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// message makes the outbox message of one tuple write, as the handle enqueues it.
func message(t *testing.T, operation Operation, tuple Tuple) Message {
	t.Helper()
	body, err := json.Marshal(payload{Version: payloadVersion, Operation: operation, Revocation: uuid.New(),
		Tuple: tupleJSON{ResourceType: tuple.Resource.Type, ResourceID: tuple.Resource.ID, Relation: tuple.Relation,
			SubjectType: tuple.Subject.Type, SubjectID: tuple.Subject.ID, SubjectRelation: tuple.SubjectRelation}})
	if err != nil {
		t.Fatal(err)
	}
	return Message{ID: uuid.New(), Destination: DestinationName, ResourceKey: tuple.Resource.String(),
		Kind: "authz." + string(operation), Payload: body}
}

func floorRevision(t *testing.T, env *pgEnv) string {
	t.Helper()
	return env.scalar(t, "SELECT revision::text FROM {s}.authz_floor")
}

func TestRelay(t *testing.T) {
	ctx := context.Background()
	viewer := Tuple{Resource: doc("1"), Relation: "viewer", Subject: user("bob")}

	t.Run("ST-SUP-RP06-LC06-T02 The relay stores the returned token on the owning row", func(t *testing.T) {
		env := newPG(t)
		if _, err := env.admin.ExecContext(ctx, "INSERT INTO "+env.schema+".documents (tenant_id, id) VALUES ('t1', '1'), ('t2', '1')"); err != nil {
			t.Fatal(err)
		}
		relay := env.relay(t, &fakeRelationships{tokens: []Token{tok(31)}})
		if err := deliver(t, relay, "t1", message(t, OperationTouch, viewer)); err != nil {
			t.Fatal(err)
		}
		if got := env.scalar(t, "SELECT authz_token FROM {s}.documents WHERE tenant_id = 't1' AND id = '1'"); got != tok(31).Text() {
			t.Fatalf("the row holds the token %q, want %q", got, tok(31).Text())
		}
		if got := env.scalar(t, "SELECT authz_token FROM {s}.documents WHERE tenant_id = 't2' AND id = '1'"); got != "" {
			t.Fatalf("the row of another tenant holds the token %q", got)
		}
	})

	t.Run("ST-SUP-RP06-LC06-T05 The authz destination is registered as idempotent and must land", func(t *testing.T) {
		relay, err := NewRelay(RelayConfig{Name: "authz-relay", Relationships: &fakeRelationships{}, DB: fakeDB(t),
			Schema: "public", Permanent: markPermanent})
		if err != nil {
			t.Fatal(err)
		}
		registration := relay.Destination()
		if registration.Name != "authz" || !registration.Idempotent || !registration.MustLand || registration.Handler == nil {
			t.Fatalf("the registration is %+v, want authz, idempotent and must land, with a handler", registration)
		}
		// A payload the relay cannot read is a permanent refusal, which the outbox parks.
		err = registration.Handler(ctx, Delivery{ID: uuid.New(), Tenant: mustTenant(t, "t1"), ResourceKey: "document:1",
			Payload: json.RawMessage(`{"v":1,"op":"create"}`)})
		var mark permanentMark
		if !errors.As(err, &mark) {
			t.Fatalf("a bad payload gave %v, want a permanent refusal", err)
		}
	})

	t.Run("ST-SUP-RP06-LC13-T01 The relay stores a delivered tuple's token as the service floor", func(t *testing.T) {
		env := newPG(t)
		relay := env.relay(t, &fakeRelationships{tokens: []Token{tok(9), tok(7), tok(11)}})
		if err := deliver(t, relay, "t1", message(t, OperationDelete, viewer)); err != nil {
			t.Fatal(err)
		}
		other := Tuple{Resource: doc("2"), Relation: "viewer", Subject: user("bob")}
		if err := deliver(t, relay, "t2", message(t, OperationTouch, other)); err != nil {
			t.Fatal(err)
		}
		if got := floorRevision(t, env); got != "9" {
			t.Fatalf("the floor is at %s after t-9 and t-7, want 9", got)
		}
		if err := deliver(t, relay, "t2", message(t, OperationTouch, other)); err != nil {
			t.Fatal(err)
		}
		if got := floorRevision(t, env); got != "11" {
			t.Fatalf("the floor is at %s after t-11, want 11", got)
		}
		floor, err := NewTableFloor(env.db, env.schema)
		if err != nil {
			t.Fatal(err)
		}
		if current, err := floor.Current(ctx); err != nil || current != tok(11) {
			t.Fatalf("the floor reads %v, %v; want t-11", current, err)
		}
	})

	t.Run("ST-SUP-RP06-LC13-T07 Concurrent relays only raise the floor", func(t *testing.T) {
		env := newPG(t)
		for run := 0; run < 20; run++ {
			if _, err := env.admin.ExecContext(ctx, "UPDATE "+env.schema+".authz_floor SET token = '', revision = NULL"); err != nil {
				t.Fatal(err)
			}
			nine, seven := env.relay(t, &fakeRelationships{tokens: []Token{tok(9)}}), env.relay(t, &fakeRelationships{tokens: []Token{tok(7)}})
			relays := []*Relay{nine, seven}
			if run%2 == 1 {
				relays = []*Relay{seven, nine}
			}
			var wg sync.WaitGroup
			errs := make([]error, 2)
			for i, relay := range relays {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs[i] = deliver(t, relay, "t"+strconv.Itoa(i+1), message(t, OperationTouch, viewer))
				}()
			}
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := floorRevision(t, env); got != "9" {
				t.Fatalf("run %d: the floor is at %s, want 9", run, got)
			}
		}
		// A relay that stopped after its floor statement holds no lock on the floor: another relay raises it at once.
		release := make(chan struct{})
		held := make(chan struct{})
		blocked := env.relay(t, &fakeRelationships{tokens: []Token{tok(20)}})
		blocked.afterFloor = func() error { close(held); <-release; return nil }
		done := make(chan error, 1)
		go func() { done <- deliver(t, blocked, "t1", message(t, OperationTouch, viewer)) }()
		<-held
		quick := env.relay(t, &fakeRelationships{tokens: []Token{tok(21)}})
		start := time.Now()
		if err := deliver(t, quick, "t2", message(t, OperationTouch, viewer)); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("the second relay waited %s for the first", elapsed)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if got := floorRevision(t, env); got != "21" {
			t.Fatalf("the floor is at %s, want 21", got)
		}
	})

	t.Run("ST-SUP-RP06-LC13-T08 The floor is raised before the entry is done", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtime(t, &recorder{}, nil)
		alice := mustPerson(t, "alice")
		tx, handle := env.begin(t, rt, "t1", alice, rel("document#viewer"))
		revocation, err := handle.Unrelate(ctx, viewer)
		if err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		fake := &fakeRelationships{tokens: []Token{tok(9), tok(9)}}
		relay := env.relay(t, fake)
		stopped := errors.New("the relay stops")
		relay.afterFloor = func() error { return stopped }
		entry := env.outbox.all()[0]
		if err := deliver(t, relay, "t1", entry); !errors.Is(err, stopped) {
			t.Fatalf("the first delivery gave %v", err)
		}
		if got := floorRevision(t, env); got != "9" {
			t.Fatalf("the floor is at %s while the entry is not done, want 9", got)
		}
		if s, err := rt.Status(ctx, mustTenant(t, "t1"), alice, revocation.ID()); err != nil || s.State != RevocationPending {
			t.Fatalf("the revocation is %+v, %v while the entry is not done, want pending", s, err)
		}
		relay.afterFloor = nil
		if err := deliver(t, relay, "t1", entry); err != nil {
			t.Fatalf("the delivery after the restart gave %v", err)
		}
		if len(fake.deletes) != 2 {
			t.Fatalf("the provider got %d deletes, want the entry delivered again", len(fake.deletes))
		}
		if got := floorRevision(t, env); got != "9" {
			t.Fatalf("the floor is at %s after the restart, want 9", got)
		}
		if s, err := rt.Status(ctx, mustTenant(t, "t1"), alice, revocation.ID()); err != nil || s.State != RevocationComplete {
			t.Fatalf("the revocation is %+v, %v after the restart, want complete", s, err)
		}
	})

	t.Run("ST-SUP-RP06-LC16-T01 The delivery alert bound defaults to 5 seconds and refuses a bound that is not positive", func(t *testing.T) {
		config := RelayConfig{Name: "authz-relay", Relationships: &fakeRelationships{}, DB: fakeDB(t), Schema: "public",
			Permanent: markPermanent}
		relay, err := NewRelay(config)
		if err != nil {
			t.Fatal(err)
		}
		if relay.AlertBound() != 5*time.Second {
			t.Fatalf("the bound is %s, want 5s", relay.AlertBound())
		}
		for _, bound := range []time.Duration{0, -time.Second} {
			if _, err := NewRelay(config, WithAlertBound(bound)); err == nil {
				t.Errorf("the bound %s is accepted", bound)
			}
		}
		if relay, err := NewRelay(config, WithAlertBound(2*time.Second)); err != nil || relay.AlertBound() != 2*time.Second {
			t.Errorf("the bound 2s gave %v, %v", relay, err)
		}
	})
}

// TestRevocationFloor proves the floor against the quantized local SpiceDB server: a group revocation, a ban and
// a revocation in another tenant are in force on a resource whose row token is older. Each case first shows that
// the provider alone, at the old row token, still reads the state before the revocation, so the deny comes from
// the floor.
func TestRevocationFloor(t *testing.T) {
	ctx := context.Background()

	// ids gives the object IDs of one case, which the cases do not share on the quantized server.
	ids := func(t *testing.T) (group, document string) {
		t.Helper()
		suffix := uuid.NewString()[:8]
		return "g-" + suffix, "d-" + suffix
	}
	// afterBoundary waits until the quantization boundary after the setup writes has passed, so that checks up to
	// the next boundary read the state at it, and returns the time left before the next boundary.
	afterBoundary := func(t *testing.T) time.Time {
		t.Helper()
		next := time.Now().Truncate(quantum).Add(quantum)
		time.Sleep(time.Until(next) + quantum/5)
		return next.Add(quantum)
	}
	inWindow := func(t *testing.T, end time.Time) {
		t.Helper()
		if time.Now().After(end.Add(-quantum / 10)) {
			t.Fatalf("the case took longer than the quantization interval %s, so its stale read proves nothing", quantum)
		}
	}
	insert := func(t *testing.T, env *pgEnv, tenantID, documentID string, token Token) {
		t.Helper()
		if _, err := env.admin.ExecContext(ctx, "INSERT INTO "+env.schema+".documents (tenant_id, id, authz_token) VALUES ($1, $2, $3)",
			tenantID, documentID, token.Text()); err != nil {
			t.Fatal(err)
		}
	}
	rowToken := func(t *testing.T, env *pgEnv, tenantID, documentID string) Token {
		t.Helper()
		token, err := ParseToken(env.scalar(t, "SELECT authz_token FROM {s}.documents WHERE tenant_id = $1 AND id = $2", tenantID, documentID))
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	view := func(t *testing.T, rt *Runtime, env *pgEnv, tenantID, documentID string) bool {
		t.Helper()
		_, handle := env.begin(t, rt, tenantID, mustPerson(t, "bob"))
		result, err := handle.Evaluate(ctx, "view", doc(documentID), rowToken(t, env, tenantID, documentID), nil)
		if err != nil {
			t.Fatal(err)
		}
		return result.Allowed
	}
	// stale checks view with the provider alone at the old row token: it must still allow, or the stand-in shows
	// no stale read and the case could not fail.
	stale := func(t *testing.T, db *spiceEnv, documentID string, old Token) {
		t.Helper()
		result, err := db.provider.Evaluate(ctx, EvaluationRequest{Subject: user("bob"), Action: Action{Name: "view"},
			Resource: doc(documentID), Consistency: AtLeast(old)})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Allowed {
			t.Fatal("the provider alone denies at the old row token: the stand-in shows no stale read, so the case cannot fail")
		}
	}
	deliverAll := func(t *testing.T, env *pgEnv, relay *Relay, tenantID string) {
		t.Helper()
		for _, m := range env.outbox.all() {
			if err := deliver(t, relay, tenantID, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	groupGrant := func(t *testing.T, db *spiceEnv, group, documentID string) (Tuple, Token) {
		t.Helper()
		member := Tuple{Resource: Resource{Type: "group", ID: group}, Relation: "member", Subject: user("bob")}
		if _, err := db.provider.Touch(ctx, []Tuple{member}, nil); err != nil {
			t.Fatal(err)
		}
		grant := Tuple{Resource: doc(documentID), Relation: "viewer", Subject: Subject{Type: "group", ID: group}, SubjectRelation: "member"}
		token, err := db.provider.Touch(ctx, []Tuple{grant}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return member, token
	}

	t.Run("ST-SUP-RP06-LC13-T03 A check after a group revocation denies on a resource whose row token is older", func(t *testing.T) {
		env, db := newPG(t), startQuantizedSpiceDB(t, fixtureSchema)
		rt, relay := env.runtime(t, db.provider, nil), env.relay(t, db.provider)
		group, d1 := ids(t)
		member, old := groupGrant(t, db, group, d1)
		insert(t, env, "t1", d1, old)
		if !view(t, rt, env, "t1", d1) {
			t.Fatal("bob cannot view the document through the group before the revocation")
		}
		end := afterBoundary(t)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("group#member"))
		if _, err := handle.Unrelate(ctx, member); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		deliverAll(t, env, relay, "t1")
		if rowToken(t, env, "t1", d1) != old {
			t.Fatal("the row token of the document changed; the case needs it older than the revocation")
		}
		stale(t, db, d1, old)
		if view(t, rt, env, "t1", d1) {
			t.Fatal("bob can view the document after the group revocation landed")
		}
		inWindow(t, end)
	})

	t.Run("ST-SUP-RP06-LC13-T04 A touch that bans a subject is in force on every resource after it lands", func(t *testing.T) {
		env, db := newPG(t), startQuantizedSpiceDB(t, fixtureSchema)
		rt, relay := env.runtime(t, db.provider, nil), env.relay(t, db.provider)
		_, d1 := ids(t)
		old, err := db.provider.Touch(ctx, []Tuple{{Resource: doc(d1), Relation: "viewer", Subject: user("bob")}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		insert(t, env, "t1", d1, old)
		if !view(t, rt, env, "t1", d1) {
			t.Fatal("bob cannot view the document before the ban")
		}
		end := afterBoundary(t)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#banned"))
		if _, err := handle.Relate(ctx, Tuple{Resource: doc(d1), Relation: "banned", Subject: user("bob")}); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		// The relay stores no token on a row here, so the row of the document keeps its older token and only the
		// floor can put the ban in force.
		relay.owners = map[string]ownerTable{}
		deliverAll(t, env, relay, "t1")
		if rowToken(t, env, "t1", d1) != old {
			t.Fatal("the row token of the document changed; the case needs it older than the ban")
		}
		stale(t, db, d1, old)
		if view(t, rt, env, "t1", d1) {
			t.Fatal("bob can view the document after the ban landed")
		}
		inWindow(t, end)
	})

	t.Run("ST-SUP-RP06-LC13-T06 A revocation in one tenant is in force for a check in another tenant", func(t *testing.T) {
		env, db := newPG(t), startQuantizedSpiceDB(t, fixtureSchema)
		rt, relay := env.runtime(t, db.provider, nil), env.relay(t, db.provider)
		group, d2 := ids(t)
		member, old := groupGrant(t, db, group, d2)
		insert(t, env, "t2", d2, old)
		if !view(t, rt, env, "t2", d2) {
			t.Fatal("bob cannot view the document of t2 before the revocation")
		}
		end := afterBoundary(t)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("group#member"))
		if _, err := handle.Unrelate(ctx, member); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		deliverAll(t, env, relay, "t1")
		stale(t, db, d2, old)
		if view(t, rt, env, "t2", d2) {
			t.Fatal("bob can view the document of t2 after the revocation in t1 landed")
		}
		inWindow(t, end)
	})
}

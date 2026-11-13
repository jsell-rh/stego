package authz

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/google/uuid"

	"github.com/jsell-rh/stego/registry/runtime/outbox"
)

// relationships reads every relationship of the resource type from the server, each as type:id#relation@type:id,
// sorted.
func (e *spiceEnv) relationships(t *testing.T, resourceType string) []string {
	t.Helper()
	stream, err := e.perms.ReadRelationships(context.Background(), &v1.ReadRelationshipsRequest{
		Consistency:        &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{ResourceType: resourceType},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			slices.Sort(out)
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		r := response.GetRelationship()
		out = append(out, r.GetResource().GetObjectType()+":"+r.GetResource().GetObjectId()+"#"+r.GetRelation()+"@"+
			r.GetSubject().GetObject().GetObjectType()+":"+r.GetSubject().GetObject().GetObjectId())
	}
}

// name gives the tuple as relationships gives it.
func name(tuple Tuple) string {
	return tuple.Resource.String() + "#" + tuple.Relation + "@" + tuple.Subject.Type + ":" + tuple.Subject.ID
}

func TestFilteredDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP10-LC05-T16 The filter of a filtered delete names one resource type, one resource ID and one relation, and the handle refuses any other", func(t *testing.T) {
		for _, c := range []struct {
			name     string
			resource Resource
			relation string
		}{
			{"no resource type", Resource{ID: doc("d1").ID}, "owner"},
			{"no resource ID", Resource{Type: "document"}, "owner"},
			{"no relation", doc("d1"), ""},
			{"a bare key", Resource{Type: "document", ID: "d1"}, "owner"},
		} {
			if _, err := RelationFilter(c.resource, c.relation); err == nil {
				t.Errorf("RelationFilter took a filter with %s", c.name)
			}
		}
		filter, err := RelationFilter(doc("d1"), "owner")
		if err != nil || !filter.OfRelation() || filter.Exact() || filter.SubjectType != "" {
			t.Fatalf("RelationFilter gave %+v, %v; want the filter of one resource and one relation", filter, err)
		}
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#owner"))
		for _, c := range []struct {
			name     string
			resource Resource
			relation string
		}{
			{"a relation the handle is not bound with", doc("d1"), "viewer"},
			{"no resource ID", Resource{Type: "document"}, "owner"},
			{"a resource of another tenant", docIn("t2", "d1"), "owner"},
			{"a bare key", Resource{Type: "document", ID: "d1"}, "owner"},
		} {
			if _, err := handle.UnrelateAll(ctx, c.resource, c.relation); err == nil {
				t.Errorf("UnrelateAll took %s", c.name)
			}
		}
		if n := env.scalar(t, "SELECT count(*) FROM stego_outbox.messages WHERE destination = 'authz'"); n != "0" {
			t.Fatalf("the refused filtered deletes enqueued %s messages", n)
		}
		viewer := Tuple{Resource: doc("d1"), Relation: "owner", Subject: user("bob")}
		first, err := handle.Relate(ctx, viewer)
		if err != nil {
			t.Fatal(err)
		}
		second, err := handle.UnrelateAll(ctx, doc("d1"), "owner")
		if err != nil {
			t.Fatal(err)
		}
		if first.ID() != second.ID() {
			t.Error("the filtered delete does not join the transaction's revocation")
		}
		env.commit(t, tx, "t1")
		ids := env.ids(t)
		if len(ids) != 2 {
			t.Fatalf("%d outbox rows for authz, want 2", len(ids))
		}
		d := env.delivery(t, ids[1])
		message, err := readPayload(d.Payload, d.Tenant)
		if err != nil || message.Operation != OperationDeleteRelation || d.Kind != "authz.delete_relation" ||
			message.Tuple.tuple() != (Tuple{Resource: doc("d1"), Relation: "owner"}) || d.ResourceKey != "document:d1" {
			t.Fatalf("the entry is %+v (%s, %s), %v; want the filtered delete of document:d1#owner", message, d.Kind, d.ResourceKey, err)
		}
		// A payload of a filtered delete that names a subject, or no resource ID, is refused by the relay.
		for _, body := range []string{
			`{"v":2,"op":"delete_relation","revocation":"` + uuid.NewString() + `","tuple":{"resource_type":"document","resource_id":"` + doc("d1").ID + `","relation":"owner","subject_type":"user","subject_id":"bob"}}`,
			`{"v":2,"op":"delete_relation","revocation":"` + uuid.NewString() + `","tuple":{"resource_type":"document","resource_id":"","relation":"owner","subject_type":"","subject_id":""}}`,
		} {
			if _, err := readPayload([]byte(body), d.Tenant); err == nil {
				t.Errorf("the relay read the payload %s", body)
			}
		}
	})

	t.Run("ST-SUP-RP10-LC05-T17 The SpiceDB adapter refuses a filtered delete with an empty resource ID or relation and deletes nothing", func(t *testing.T) {
		db := startSpiceDB(t, fixtureSchema)
		tuples := []Tuple{
			{Resource: doc("d1"), Relation: "owner", Subject: user("u1")},
			{Resource: doc("d2"), Relation: "owner", Subject: user("u1")},
			{Resource: doc("d1"), Relation: "viewer", Subject: user("u1")},
		}
		if _, err := db.provider.Touch(ctx, tuples, nil); err != nil {
			t.Fatal(err)
		}
		before := db.relationships(t, "document")
		for _, filter := range []Filter{
			{ResourceType: "document"},
			{ResourceType: "document", Relation: "owner"},
			{ResourceType: "document", ResourceID: doc("d1").ID},
			{ResourceType: "document", Relation: "owner", SubjectType: "user", SubjectID: "u1"},
			{ResourceType: "document", ResourceID: doc("d1").ID, Relation: "owner", SubjectType: "user"},
		} {
			_, err := db.provider.Delete(ctx, []Filter{filter}, nil)
			if err == nil || !IsRefused(err) {
				t.Errorf("Delete of %+v gave %v, want a refusal", filter, err)
			}
		}
		if after := db.relationships(t, "document"); !slices.Equal(before, after) {
			t.Fatalf("the refused deletes changed the relationships from %v to %v", before, after)
		}
	})

	t.Run("ST-SUP-RP10-LC05-T18 A filtered delete of row A, landed through the relay, leaves every tuple of row B", func(t *testing.T) {
		env, db := newPG(t), startSpiceDB(t, fixtureSchema)
		rt, relay := env.runtimeOf(t, db.provider, nil), env.relay(t, db.provider)
		suffix := uuid.NewString()[:8]
		a, b := "d-"+suffix, "d-"+suffix+"0" // B's key starts with A's key.
		kept := []Tuple{
			{Resource: doc(a), Relation: "viewer", Subject: user("u1")},
			{Resource: doc(b), Relation: "owner", Subject: user("u1")},
			{Resource: doc(b), Relation: "owner", Subject: user("u2")},
			{Resource: doc(b), Relation: "viewer", Subject: user("u1")},
			{Resource: docIn("t2", a), Relation: "owner", Subject: user("u1")},
		}
		gone := []Tuple{
			{Resource: doc(a), Relation: "owner", Subject: user("u1")},
			{Resource: doc(a), Relation: "owner", Subject: user("u2")},
		}
		if _, err := db.provider.Touch(ctx, append(slices.Clone(kept), gone...), nil); err != nil {
			t.Fatal(err)
		}
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#owner"))
		if _, err := handle.UnrelateAll(ctx, doc(a), "owner"); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		env.startWorker(t, relay)
		env.waitAll(t, outbox.Done)
		var want []string
		for _, tuple := range kept {
			want = append(want, name(tuple))
		}
		slices.Sort(want)
		if got := db.relationships(t, "document"); !slices.Equal(got, want) {
			t.Fatalf("after the filtered delete of %s#owner the relationships are %v, want %v", a, got, want)
		}
		if token := env.scalar(t, "SELECT token FROM "+env.schema+".authz_floor"); strings.TrimSpace(token) == "" {
			t.Error("the filtered delete did not raise the floor")
		}
	})
}

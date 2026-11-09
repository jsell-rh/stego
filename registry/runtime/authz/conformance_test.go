package authz

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// memProvider is an in-memory fake provider of both ports. view is given by owner or viewer, edit by owner. When
// stale is set, it ignores the consistency and answers from a copy that a delete does not reach. When allowAll is
// set, it allows every request.
type memProvider struct {
	mu       sync.Mutex
	tuples   map[Tuple]bool
	copy     map[Tuple]bool
	stale    bool
	allowAll bool
	revision int
}

func newMem() *memProvider { return &memProvider{tuples: map[Tuple]bool{}, copy: map[Tuple]bool{}} }

var memPermissions = map[string][]string{"view": {"owner", "viewer"}, "edit": {"owner"}}

func (m *memProvider) Evaluate(_ context.Context, request EvaluationRequest) (EvaluationResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.tuples
	if m.stale {
		state = m.copy
	}
	allowed := m.allowAll
	for _, relation := range memPermissions[request.Action.Name] {
		allowed = allowed || state[Tuple{Resource: request.Resource, Relation: relation, Subject: request.Subject}]
	}
	return EvaluationResult{Allowed: allowed, Token: tok(m.revision)}, nil
}

func (m *memProvider) EvaluateBatch(ctx context.Context, request BatchRequest) ([]EvaluationResult, error) {
	var results []EvaluationResult
	for _, evaluation := range request.Evaluations {
		result, err := m.Evaluate(ctx, evaluation)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (m *memProvider) Search(context.Context, SearchRequest) (SearchResult, error) {
	return SearchResult{}, ErrNoSearch
}

func (m *memProvider) Searches() bool { return false }

func (m *memProvider) Touch(_ context.Context, tuples []Tuple, _ []Precondition) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tuple := range tuples {
		m.tuples[tuple] = true
		m.copy[tuple] = true
	}
	m.revision++
	return tok(m.revision), nil
}

func (m *memProvider) Delete(_ context.Context, filters []Filter, _ []Precondition) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for tuple := range m.tuples {
		for _, filter := range filters {
			if FilterOf(tuple) == filter {
				delete(m.tuples, tuple)
			}
		}
	}
	m.revision++
	return tok(m.revision), nil
}

// suitePermissions are the permissions view and edit of the fixture schema, each granted on its own document so a
// grant of one does not give the other.
var suitePermissions = []Permission{
	{Name: "view", Grant: Tuple{Resource: doc("c1"), Relation: "viewer", Subject: user("alice")}, Stranger: user("mallory")},
	{Name: "edit", Grant: Tuple{Resource: doc("c2"), Relation: "owner", Subject: user("carol")}, Stranger: user("mallory")},
}

func caseOf(t *testing.T, report Report, name string) CaseResult {
	t.Helper()
	for _, c := range append(append([]CaseResult(nil), report.Cases...), report.Search...) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the report has no case %q: %+v", name, report)
	return CaseResult{}
}

func TestConformance(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP06-LC11-T01 The suite runs one allow and one deny for each permission", func(t *testing.T) {
		db := startSpiceDB(t, fixtureSchema)
		report, err := Conform(ctx, Suite{Decision: db.provider, Relationships: db.provider, Permissions: suitePermissions})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"allow view", "deny view", "allow edit", "deny edit"} {
			if c := caseOf(t, report, name); !c.Passed {
				t.Errorf("SpiceDB failed %s: %s", name, c.Detail)
			}
		}
		if !report.Passed() {
			t.Errorf("SpiceDB failed the suite: %+v", report)
		}
		permissive := newMem()
		permissive.allowAll = true
		report, err = Conform(ctx, Suite{Decision: permissive, Relationships: permissive, Permissions: suitePermissions})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"deny view", "deny edit"} {
			if caseOf(t, report, name).Passed {
				t.Errorf("a provider that allows everything passed %s", name)
			}
		}
		if report.Passed() {
			t.Error("a provider that allows everything passed the suite")
		}
	})

	t.Run("ST-SUP-RP06-LC11-T02 The new-enemy case denies at the token of a removed relation", func(t *testing.T) {
		db := startSpiceDB(t, fixtureSchema)
		report, err := Conform(ctx, Suite{Decision: db.provider, Relationships: db.provider, Permissions: suitePermissions})
		if err != nil {
			t.Fatal(err)
		}
		if c := caseOf(t, report, "new enemy view"); !c.Passed {
			t.Errorf("SpiceDB failed the new-enemy case: %s", c.Detail)
		}
		stale := newMem()
		stale.stale = true
		report, err = Conform(ctx, Suite{Decision: stale, Relationships: stale, Permissions: suitePermissions})
		if err != nil {
			t.Fatal(err)
		}
		if caseOf(t, report, "new enemy view").Passed {
			t.Error("a provider that answers from a stale copy passed the new-enemy case")
		}
		if !caseOf(t, report, "allow view").Passed || !caseOf(t, report, "deny view").Passed {
			t.Error("the stale fake fails more than the new-enemy case, so the case does not show the stale read")
		}
	})

	t.Run("ST-SUP-RP06-LC11-T03 A provider that does not search skips the search group", func(t *testing.T) {
		mem := newMem()
		report, err := Conform(ctx, Suite{Decision: mem, Relationships: mem, Permissions: suitePermissions})
		if err != nil {
			t.Fatal(err)
		}
		if report.Searches || len(report.Search) != 0 {
			t.Fatalf("the search group ran: %+v", report.Search)
		}
		if !report.Passed() {
			t.Fatalf("the suite failed: %+v", report)
		}
		if !strings.Contains(report.Note, "does not search") {
			t.Fatalf("the report says %q, want that the provider does not search", report.Note)
		}
	})
}

package authz

import (
	"context"
	"encoding/base64"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// recorder is the recording fake provider of the tests: it stores each request it gets and answers from its rules.
// By default it denies every request, returns the token t-1 and does not search.
type recorder struct {
	mu          sync.Mutex
	evaluations []EvaluationRequest
	batches     []BatchRequest
	searchCalls []SearchRequest
	// allow decides one request; nil denies all.
	allow func(EvaluationRequest) bool
	// token gives the token of the n-th call, from 1; nil gives t-1.
	token func(n int) Token
	// search is the result of a search; it searches only when it is set.
	search func(SearchRequest) SearchResult
	calls  int
}

func (r *recorder) next() Token {
	r.calls++
	if r.token == nil {
		return tok(1)
	}
	return r.token(r.calls)
}

func (r *recorder) decide(request EvaluationRequest) bool {
	return r.allow != nil && r.allow(request)
}

func (r *recorder) Evaluate(_ context.Context, request EvaluationRequest) (EvaluationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evaluations = append(r.evaluations, request)
	return EvaluationResult{Allowed: r.decide(request), Token: r.next(), Reason: "fake"}, nil
}

func (r *recorder) EvaluateBatch(_ context.Context, request BatchRequest) ([]EvaluationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, request)
	token := r.next()
	var results []EvaluationResult
	for _, evaluation := range request.Evaluations {
		results = append(results, EvaluationResult{Allowed: r.decide(evaluation), Token: token, Reason: "fake"})
	}
	return results, nil
}

func (r *recorder) Search(_ context.Context, request SearchRequest) (SearchResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.searchCalls = append(r.searchCalls, request)
	if r.search == nil {
		return SearchResult{}, ErrNoSearch
	}
	result := r.search(request)
	result.Token = r.next()
	return result, nil
}

func (r *recorder) Searches() bool { return r.search != nil }

func (r *recorder) requests() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.evaluations) + len(r.batches) + len(r.searchCalls)
}

// tok is the fake token t-n.
func tok(n int) Token { return named("t", n) }

// floorSeq is a fake revocation floor that gives the next of its tokens at each read, and the last one after.
type floorSeq struct {
	mu     sync.Mutex
	tokens []Token
	reads  int
}

func (f *floorSeq) Current(context.Context) (Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if len(f.tokens) == 0 {
		return Token{}, nil
	}
	if f.reads > len(f.tokens) {
		return f.tokens[len(f.tokens)-1], nil
	}
	return f.tokens[f.reads-1], nil
}

func fixedFloor(token Token) *floorSeq { return &floorSeq{tokens: []Token{token}} }

func fieldNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		names = append(names, t.Field(i).Name)
	}
	return names
}

func exportedFields(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).IsExported() {
			names = append(names, t.Field(i).Name)
		}
	}
	return names
}

func sameSet(got []string, want ...string) bool {
	got = append([]string(nil), got...)
	want = append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(want)
	return reflect.DeepEqual(got, want)
}

func user(id string) Subject { return Subject{Type: "user", ID: id} }

func doc(id string) Resource { return Resource{Type: "document", ID: id} }

// packageSource parses the package files that are not tests.
func packageSource(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	if len(files) < 6 {
		t.Fatalf("found %d source files of the package, want at least 6", len(files))
	}
	return fset, files
}

// identifiersAndStrings returns every identifier and the value of every string literal of the files.
func identifiersAndStrings(t *testing.T, files []*ast.File) (identifiers, literals []string) {
	t.Helper()
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				identifiers = append(identifiers, n.Name)
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					value, err := strconv.Unquote(n.Value)
					if err != nil {
						t.Fatal(err)
					}
					literals = append(literals, value)
				}
			}
			return true
		})
	}
	return identifiers, literals
}

func TestDecisionPort(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP06-LC01-T01 The evaluation takes one subject, action, resource, context and consistency and returns allowed, token and reason", func(t *testing.T) {
		if got := fieldNames(reflect.TypeOf(EvaluationRequest{})); !sameSet(got, "Subject", "Action", "Resource", "Context", "Consistency") {
			t.Errorf("EvaluationRequest holds %v", got)
		}
		if got := fieldNames(reflect.TypeOf(EvaluationResult{})); !sameSet(got, "Allowed", "Token", "Reason") {
			t.Errorf("EvaluationResult holds %v", got)
		}
		port := reflect.TypeOf((*Decision)(nil)).Elem()
		evaluate, ok := port.MethodByName("Evaluate")
		if !ok {
			t.Fatal("the Decision interface has no Evaluate")
		}
		if evaluate.Type.NumIn() != 2 || evaluate.Type.In(1) != reflect.TypeOf(EvaluationRequest{}) ||
			evaluate.Type.Out(0) != reflect.TypeOf(EvaluationResult{}) {
			t.Errorf("Evaluate is %s, want func(context.Context, EvaluationRequest) (EvaluationResult, error)", evaluate.Type)
		}
		for _, name := range []string{"EvaluateBatch", "Search", "Searches"} {
			if _, ok := port.MethodByName(name); !ok {
				t.Errorf("the Decision interface has no %s", name)
			}
		}
	})

	t.Run("ST-SUP-RP06-LC01-T02 A batch evaluation runs every request when no semantic is given", func(t *testing.T) {
		fake := &recorder{allow: func(r EvaluationRequest) bool { return r.Resource.ID != "1" }}
		port, err := NewPort(fake, nil)
		if err != nil {
			t.Fatal(err)
		}
		var batch BatchRequest
		for _, id := range []string{"1", "2", "3"} {
			batch.Evaluations = append(batch.Evaluations, EvaluationRequest{Subject: user("bob"),
				Action: Action{Name: "view"}, Resource: doc(id), Consistency: Fresh()})
		}
		results, err := port.EvaluateBatch(ctx, batch)
		if err != nil {
			t.Fatal(err)
		}
		if len(fake.batches) != 1 || fake.batches[0].Semantic != ExecuteAll {
			t.Fatalf("the provider got %+v, want one batch with the semantic execute_all", fake.batches)
		}
		if len(results) != 3 || results[0].Allowed || !results[1].Allowed || !results[2].Allowed {
			t.Fatalf("results %+v, want a deny and then two allows evaluated after it", results)
		}
	})

	t.Run("ST-SUP-RP06-LC01-T03 A resource search takes subject, action, resource type, context, consistency and page, and returns an opaque page token", func(t *testing.T) {
		if got := fieldNames(reflect.TypeOf(SearchRequest{})); !sameSet(got, "Subject", "Action", "ResourceType", "Context", "Consistency", "Page") {
			t.Errorf("SearchRequest holds %v", got)
		}
		if got := exportedFields(reflect.TypeOf(PageToken{})); len(got) != 0 {
			t.Errorf("PageToken has the exported fields %v", got)
		}
		fake := &recorder{search: func(SearchRequest) SearchResult {
			return SearchResult{Resources: []Resource{doc("1")}, Next: NewPageToken("fake", "cursor-1", Fresh())}
		}}
		port, err := NewPort(fake, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := port.Search(ctx, SearchRequest{Subject: user("bob"), Action: Action{Name: "view"},
			ResourceType: "document", Consistency: Fresh(), Page: Page{Limit: 1}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Next.IsZero() {
			t.Fatal("the search gave no next page")
		}
		if cursor, first, ok := result.Next.CursorFor("fake"); !ok || cursor != "cursor-1" || !first.IsFresh() {
			t.Errorf("the provider reads the cursor %q, %s, %t", cursor, first, ok)
		}
		if _, _, ok := result.Next.CursorFor("spicedb"); ok {
			t.Error("another provider reads the page token")
		}
	})

	t.Run("ST-SUP-RP06-LC01-T04 A deny is a decision with allowed false and no error", func(t *testing.T) {
		db := startSpiceDB(t, fixtureSchema)
		port, err := NewPort(db.provider, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := port.Evaluate(ctx, EvaluationRequest{Subject: user("nobody"), Action: Action{Name: "view"},
			Resource: doc("1"), Consistency: Fresh()})
		if err != nil {
			t.Fatalf("a deny gave the error %v", err)
		}
		if result.Allowed || result.Token.IsZero() {
			t.Fatalf("result %+v, want allowed false with the token of the check", result)
		}
	})
}

func TestRelationshipsPort(t *testing.T) {
	ctx := context.Background()
	viewer := Tuple{Resource: doc("1"), Relation: "viewer", Subject: user("bob")}

	t.Run("ST-SUP-RP06-LC02-T01 Touch and Delete take preconditions and are idempotent on retry", func(t *testing.T) {
		port := reflect.TypeOf((*Relationships)(nil)).Elem()
		ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
		for name, first := range map[string]reflect.Type{"Touch": reflect.TypeOf([]Tuple{}), "Delete": reflect.TypeOf([]Filter{})} {
			method, ok := port.MethodByName(name)
			if !ok {
				t.Fatalf("Relationships has no %s", name)
			}
			in := method.Type
			if in.NumIn() != 3 || in.In(0) != ctxType || in.In(1) != first || in.In(2) != reflect.TypeOf([]Precondition{}) ||
				in.NumOut() != 2 || in.Out(0) != reflect.TypeOf(Token{}) {
				t.Errorf("%s is %s, want func(context.Context, %s, []Precondition) (Token, error)", name, in, first)
			}
		}
		db := startSpiceDB(t, fixtureSchema)
		for range 2 {
			if _, err := db.provider.Touch(ctx, []Tuple{viewer}, nil); err != nil {
				t.Fatalf("touch: %v", err)
			}
		}
		if n := db.count(t, "document"); n != 1 {
			t.Fatalf("%d relationships after two touches, want 1", n)
		}
		for range 2 {
			if _, err := db.provider.Delete(ctx, []Filter{FilterOf(viewer)}, nil); err != nil {
				t.Fatalf("delete: %v", err)
			}
		}
		if n := db.count(t, "document"); n != 0 {
			t.Fatalf("%d relationships after two deletes, want 0", n)
		}
	})

	t.Run("ST-SUP-RP06-LC02-T02 Touch and Delete return the token the provider wrote at", func(t *testing.T) {
		db := startSpiceDB(t, fixtureSchema)
		port, err := NewPort(db.provider, nil)
		if err != nil {
			t.Fatal(err)
		}
		check := func(token Token) bool {
			t.Helper()
			result, err := port.Evaluate(ctx, EvaluationRequest{Subject: user("bob"), Action: Action{Name: "view"},
				Resource: doc("1"), Consistency: AtLeast(token)})
			if err != nil {
				t.Fatal(err)
			}
			return result.Allowed
		}
		touched, err := db.provider.Touch(ctx, []Tuple{viewer}, nil)
		if err != nil || touched.IsZero() {
			t.Fatalf("touch gave %v, %v", touched, err)
		}
		if !check(touched) {
			t.Fatal("view is denied at the token of the touch")
		}
		deleted, err := db.provider.Delete(ctx, []Filter{FilterOf(viewer)}, nil)
		if err != nil || deleted.IsZero() {
			t.Fatalf("delete gave %v, %v", deleted, err)
		}
		if check(deleted) {
			t.Fatal("view is allowed at the token of the delete")
		}
	})
}

func TestConsistency(t *testing.T) {
	t.Run("ST-SUP-RP06-LC03-T01 A consistency is either fresh or at least as fresh as a token", func(t *testing.T) {
		_, files := packageSource(t)
		var makers []string
		for _, file := range files {
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Recv != nil || !d.Name.IsExported() || d.Type.Results == nil {
						continue
					}
					for _, result := range d.Type.Results.List {
						if ident, ok := result.Type.(*ast.Ident); ok && ident.Name == "Consistency" {
							makers = append(makers, d.Name.Name)
						}
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						value, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for _, name := range value.Names {
							if ident, ok := value.Type.(*ast.Ident); ok && ident.Name == "Consistency" && name.IsExported() {
								makers = append(makers, name.Name)
							}
						}
					}
				}
			}
		}
		if !sameSet(makers, "Fresh", "AtLeast") {
			t.Fatalf("the exported constructors and values of Consistency are %v, want exactly Fresh and AtLeast", makers)
		}
		if !Fresh().IsFresh() {
			t.Error("Fresh is not fresh")
		}
		if got, ok := AtLeast(tok(4)).Token(); !ok || got != tok(4) {
			t.Errorf("AtLeast(t-4) holds %v, %t", got, ok)
		}
		if got := exportedFields(reflect.TypeOf(Consistency{})); len(got) != 0 {
			t.Errorf("Consistency has the exported fields %v", got)
		}
	})

	t.Run("ST-SUP-RP06-LC03-T02 No minimize-latency or exact-snapshot option exists", func(t *testing.T) {
		_, files := packageSource(t)
		identifiers, literals := identifiersAndStrings(t, files)
		forbidden := []string{"minimizelatency", "minimize_latency", "exactsnapshot", "exact_snapshot", "atexactsnapshot", "at_exact_snapshot"}
		for _, word := range append(identifiers, literals...) {
			folded := strings.ToLower(strings.ReplaceAll(word, " ", "_"))
			for _, bad := range forbidden {
				if strings.Contains(folded, bad) {
					t.Errorf("the package holds %q, which names %s", word, bad)
				}
			}
		}
	})

	t.Run("ST-SUP-RP06-LC03-T03 An evaluation with a zero consistency is refused before the provider is called", func(t *testing.T) {
		fake := &recorder{allow: func(EvaluationRequest) bool { return true }}
		port, err := NewPort(fake, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = port.Evaluate(context.Background(), EvaluationRequest{Subject: user("bob"), Action: Action{Name: "view"},
			Resource: doc("1")})
		if !errors.Is(err, ErrNoConsistency) || !strings.Contains(err.Error(), "consistency") {
			t.Fatalf("the zero consistency gave %v, want an error that names the consistency", err)
		}
		_, err = port.EvaluateBatch(context.Background(), BatchRequest{Evaluations: []EvaluationRequest{{
			Subject: user("bob"), Action: Action{Name: "view"}, Resource: doc("1")}}})
		if !errors.Is(err, ErrNoConsistency) {
			t.Fatalf("a batch with the zero consistency gave %v", err)
		}
		if n := fake.requests(); n != 0 {
			t.Fatalf("the provider got %d requests", n)
		}
	})
}

func TestToken(t *testing.T) {
	t.Run("ST-SUP-RP06-LC04-T01 A token round-trips through its text column form and is never compared", func(t *testing.T) {
		token, err := NewToken("GhUKEzE3MzAwMDAwMDAwMDAwMDAwMDA=")
		if err != nil {
			t.Fatal(err)
		}
		text := token.Text()
		if text == "" {
			t.Fatal("the text form is empty")
		}
		back, err := ParseToken(text)
		if err != nil || back != token || back.Value() != "GhUKEzE3MzAwMDAwMDAwMDAwMDAwMDA=" {
			t.Fatalf("the token read back is %q, %v", back.Text(), err)
		}
		if got := exportedFields(reflect.TypeOf(Token{})); len(got) != 0 {
			t.Errorf("Token has the exported fields %v", got)
		}
		// No exported method or function orders, compares or merges two tokens: none takes two tokens or a token
		// and another token's holder, and none is named so.
		_, files := packageSource(t)
		tokenType := func(expr ast.Expr) bool {
			ident, ok := expr.(*ast.Ident)
			return ok && ident.Name == "Token"
		}
		for _, file := range files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || !fn.Name.IsExported() {
					continue
				}
				tokens := 0
				if fn.Recv != nil && len(fn.Recv.List) == 1 && tokenType(fn.Recv.List[0].Type) {
					tokens++
				}
				for _, param := range fn.Type.Params.List {
					if tokenType(param.Type) {
						tokens += max(1, len(param.Names))
					}
				}
				lower := strings.ToLower(fn.Name.Name)
				for _, word := range []string{"fresher", "compare", "less", "newer", "older", "merge", "max", "min", "order"} {
					if strings.Contains(lower, word) {
						t.Errorf("the package exports %s", fn.Name.Name)
					}
				}
				if tokens >= 2 {
					t.Errorf("%s takes two tokens", fn.Name.Name)
				}
			}
		}
	})

	t.Run("ST-SUP-RP06-LC04-T04 Tokens of each datastore's form are used without being parsed", func(t *testing.T) {
		forms := map[string]string{
			"decimal (memdb, mysql, spanner)": zed("1730000000000000000"),
			"hybrid logical clock (crdb)":     zed("1730000000000000000.0000000001"),
			"PostgreSQL snapshot":             zed(base64.StdEncoding.EncodeToString([]byte{0x08, 0xd2, 0x09, 0x10, 0xd8, 0x09, 0x1a, 0x02, 0xd3, 0x09})),
		}
		for name, value := range forms {
			token, err := NewToken(value)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			back, err := ParseToken(token.Text())
			if err != nil || back.Text() != value {
				t.Fatalf("%s: the token read back is %q, %v", name, back.Text(), err)
			}
			fake := &recorder{}
			rt := newUnitRuntime(t, fake)
			person := mustPerson(t, "u-7")
			// As a row token, while the floor is zero.
			if _, err := bindUnit(t, rt, nil, person).Evaluate(context.Background(), "view", doc("1"), back, nil); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			// As the floor.
			if _, err := bindUnit(t, rt, fixedFloor(back), person).Evaluate(context.Background(), "view", doc("1"), Token{}, nil); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for i, request := range fake.evaluations {
				if got := atLeast(t, request.Consistency); got.Value() != value {
					t.Errorf("%s: request %d sent %q, want the token byte for byte", name, i, got.Value())
				}
			}
		}
	})
}

// reflectType returns the type of v, or the type it points to.
func reflectType(v any) reflect.Type {
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Pointer {
		return t.Elem()
	}
	return t
}

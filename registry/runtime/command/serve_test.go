package command

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/otel"
)

// serveCase is one fill outcome of the Serve test: what the fill does, and the answer and audit row it gives.
type serveCase struct {
	name     string
	resource string
	fill     func(ctx context.Context, h *Handle[testProbe]) error
	status   int
	code     string
	rpc      codes.Code
	outcome  string
	auditErr string
}

func TestServe(t *testing.T) {
	t.Run("ST-SUP-RP10-LC33-T01 Serve and ServeRPC map each outcome of Run to its answer and audit row", func(t *testing.T) {
		e := newEnv(t)
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o5', 'open'), ('t1', 'o6', 'open')`)
		cmd := e.command("reserve", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "reserve", orderResource()
		}, e.reservationE)
		shortStock := NewRefusal("reserve", "short_stock", http.StatusUnprocessableEntity, "reserve.short_stock")
		const secret = "jane.doe@example.com"
		var current func(ctx context.Context, h *Handle[testProbe]) error
		spec := ServeSpec[*structpb.Struct, *structpb.Struct, testProbe]{
			Command: cmd, ResourceType: "order", SubjectType: "user", Status: http.StatusCreated,
			NewRequest:  func() *structpb.Struct { return &structpb.Struct{} },
			NewResponse: func() *structpb.Struct { return &structpb.Struct{} },
			Resource:    func(r *structpb.Struct) string { return r.GetFields()["order_id"].GetStringValue() },
			Fill: func(ctx context.Context, r *structpb.Struct, h *Handle[testProbe]) (*structpb.Struct, error) {
				if err := current(ctx, h); err != nil {
					return nil, err
				}
				return structpb.NewStruct(map[string]any{"done": true})
			},
			Refusals: []*Refusal{shortStock}, KeyRequired: true,
		}
		h, err := Serve(e.rt, spec)
		if err != nil {
			t.Fatal(err)
		}
		rpc, err := ServeRPC(e.rt, spec)
		if err != nil {
			t.Fatal(err)
		}
		key := authntest.NewKey(t, "k1")
		set, err := authn.ParseKeys(authntest.Document(key.JWK()))
		if err != nil {
			t.Fatal(err)
		}
		verifier, err := authn.New(authn.Settings{Issuer: "https://issuer.example", Audience: "orders", ProblemCode: "ORDERS",
			Keys: set, TenantClaim: "tenant"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(verifier.Stop)
		claims := authntest.Claims("https://issuer.example", "orders", "u-7", time.Now())
		claims["tenant"] = "t1"
		token := authntest.Mint(t, authntest.Header("k1"), claims, key)
		identity, err := verifier.Verify(context.Background(), token)
		if err != nil {
			t.Fatal(err)
		}
		caller, err := authz.Person("user", identity)
		if err != nil {
			t.Fatal(err)
		}
		// The caller may reserve on o5 only.
		e.decision.setRelations(relation(t, caller.Subject().String(), "reserve", "t1", "order", "o5"))

		cases := []serveCase{
			{name: "commit", resource: "o5", status: http.StatusCreated, rpc: codes.OK, outcome: "ok",
				fill: func(ctx context.Context, h *Handle[testProbe]) error {
					_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-serve-" + NewID(), OrderID: "o5", SKU: "s"}, nil)
					return err
				}},
			{name: "declared refusal", resource: "o5", status: http.StatusUnprocessableEntity, code: "reserve.short_stock",
				rpc: codes.InvalidArgument, outcome: "failed", auditErr: "reserve.short_stock",
				fill: func(context.Context, *Handle[testProbe]) error { return shortStock }},
			{name: "deny", resource: "o6", status: http.StatusNotFound, code: "denied", rpc: codes.NotFound, outcome: "denied",
				auditErr: "denied", fill: func(context.Context, *Handle[testProbe]) error { return nil }},
			{name: "conflict", resource: "o5", status: http.StatusConflict, code: "conflict", rpc: codes.Aborted,
				outcome: "failed", auditErr: "conflict",
				fill: func(context.Context, *Handle[testProbe]) error { return &conflictError{text: "moved"} }},
			{name: "commit of unknown outcome", resource: "o5", status: http.StatusInternalServerError, code: "internal",
				rpc: codes.Internal, outcome: "failed", auditErr: "commit_outcome_unknown",
				fill: func(context.Context, *Handle[testProbe]) error { return ErrCommitUnknown }},
			{name: "another error", resource: "o5", status: http.StatusInternalServerError, code: "internal",
				rpc: codes.Internal, outcome: "failed", auditErr: "internal",
				fill: func(context.Context, *Handle[testProbe]) error { return errors.New("no stock for " + secret) }},
			{name: "a refusal that the fill made itself", resource: "o5", status: http.StatusInternalServerError,
				code: "internal", rpc: codes.Internal, outcome: "failed", auditErr: "internal",
				fill: func(context.Context, *Handle[testProbe]) error {
					return NewRefusal("reserve", "short_stock", http.StatusUnprocessableEntity, "reserve.short_stock")
				}},
		}
		lastAudit := func() (outcome, code string) {
			t.Helper()
			if err := e.admin.QueryRowContext(e.ctx, `SELECT outcome, coalesce(error_code, '') FROM audit_event
 WHERE tenant_id = 't1' AND action = 'reserve' ORDER BY seq DESC LIMIT 1`).Scan(&outcome, &code); err != nil {
				t.Fatal(err)
			}
			return outcome, code
		}
		for i, c := range cases {
			current = c.fill
			before := e.auditRows("t1", "reserve")
			// HTTP, through the verifier, with a place for the code of the completion log.
			body := `{"order_id":"` + c.resource + `"}`
			r := httptest.NewRequest(http.MethodPost, "/commands/reserve", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Idempotency-Key", `"k-serve-http-`+string(rune('a'+i))+`"`)
			w := httptest.NewRecorder()
			var logged string
			verifier.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := otel.WithErrorCode(r.Context())
				h.ServeHTTP(w, r.WithContext(ctx))
				logged, _ = otel.ErrorCode(ctx)
			})).ServeHTTP(w, r)
			answer, _ := io.ReadAll(w.Result().Body)
			if w.Code != c.status || strings.Contains(string(answer), secret) {
				t.Fatalf("%s over HTTP gave %d %s, want %d with no error text", c.name, w.Code, answer, c.status)
			}
			if c.code != "" {
				var p map[string]any
				if err := json.Unmarshal(answer, &p); err != nil || p["code"] != c.code || len(p) != 4 || logged != c.code {
					t.Fatalf("%s over HTTP gave the problem %s and the log code %q, want the code %s", c.name, answer, logged, c.code)
				}
			} else if logged != "" {
				t.Fatalf("%s over HTTP gave the log code %q, want none", c.name, logged)
			}
			if outcome, code := lastAudit(); e.auditRows("t1", "reserve") != before+1 || outcome != c.outcome || code != c.auditErr {
				t.Fatalf("%s over HTTP left the audit row %s %q (%d rows), want one row %s %q", c.name, outcome, code,
					e.auditRows("t1", "reserve")-before, c.outcome, c.auditErr)
			}

			// gRPC, with the identity of authn.Authenticate and the key in the request metadata.
			ctx, err := verifier.Authenticate(context.Background(), "Bearer "+token)
			if err != nil {
				t.Fatal(err)
			}
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(RPCKeyMetadata, `"k-serve-rpc-`+string(rune('a'+i))+`"`))
			request, _ := structpb.NewStruct(map[string]any{"order_id": c.resource})
			before = e.auditRows("t1", "reserve")
			response, err := rpc(ctx, request)
			if status.Code(err) != c.rpc || (err != nil && strings.Contains(err.Error(), secret)) {
				t.Fatalf("%s over gRPC gave %v, want %s with no error text", c.name, err, c.rpc)
			}
			if err == nil && !response.GetFields()["done"].GetBoolValue() {
				t.Fatalf("%s over gRPC gave the response %v", c.name, response)
			}
			if c.code != "" && status.Convert(err).Message() != c.code {
				t.Fatalf("%s over gRPC gave the message %q, want the code %s", c.name, status.Convert(err).Message(), c.code)
			}
			if outcome, code := lastAudit(); e.auditRows("t1", "reserve") != before+1 || outcome != c.outcome || code != c.auditErr {
				t.Fatalf("%s over gRPC left the audit row %s %q, want %s %q", c.name, outcome, code, c.outcome, c.auditErr)
			}
		}

		// A request with no token never reaches Run.
		calls := 0
		current = func(context.Context, *Handle[testProbe]) error { calls++; return nil }
		before := e.auditRows("t1", "")
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/commands/reserve", strings.NewReader(`{"order_id":"o5"}`))
		r.Header.Set("Idempotency-Key", `"k-serve-none"`)
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("a request with no token gave %d, want 401", w.Code)
		}
		if _, err := rpc(context.Background(), &structpb.Struct{}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("a call with no token gave %v, want UNAUTHENTICATED", err)
		}
		if calls != 0 || e.auditRows("t1", "") != before {
			t.Fatal("a request with no token reached the fill or wrote an audit row")
		}
		// A required key that is missing is 400, and a spec that lacks a part is refused.
		r = httptest.NewRequest(http.MethodPost, "/commands/reserve", strings.NewReader(`{"order_id":"o5"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		w = httptest.NewRecorder()
		verifier.Middleware(h).ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || calls != 0 {
			t.Fatalf("a request with no key gave %d, want 400 with no fill call", w.Code)
		}
		broken := spec
		broken.Fill = nil
		if _, err := Serve(e.rt, broken); err == nil {
			t.Fatal("Serve took a spec with no fill")
		}
	})

	t.Run("ST-SUP-RP10-LC33-T01 The contract helpers that never change", func(t *testing.T) {
		seen := map[string]bool{}
		for range 100 {
			id := NewID()
			if !ValidUUID(id) || seen[id] || id[14] != '4' {
				t.Fatalf("NewID gave %q", id)
			}
			seen[id] = true
		}
		for _, bad := range []string{"", "o5", "0a1b2c3d-0a1b-4c3d-8e9f-0a1b2c3d4e5", "0a1b2c3d_0a1b-4c3d-8e9f-0a1b2c3d4e5f", "0a1b2c3d-0a1b-4c3d-8e9f-0a1b2c3d4e5g"} {
			if ValidUUID(bad) {
				t.Fatalf("ValidUUID took %q", bad)
			}
		}
		n := 5
		p := ClonePointer(&n)
		*p = 6
		if n != 5 || ClonePointer[int](nil) != nil || StateOf(&n) != any(5) || StateOf[int](nil) != nil {
			t.Fatal("ClonePointer or StateOf is wrong")
		}
		m := 5
		if !EqualPointer(&n, &m) || EqualPointer(&n, nil) || !EqualPointer[int](nil, nil) {
			t.Fatal("EqualPointer is wrong")
		}
		var field string
		if SetPath(&field, "o5") != nil || field != "o5" || !errors.Is(SetPath(&field, "o6"), ErrBadRequest) || !errors.Is(SetPath(&field, ""), ErrBadRequest) {
			t.Fatal("SetPath is wrong")
		}
		defer func() {
			if recover() == nil {
				t.Fatal("Must did not panic on an error")
			}
		}()
		Must(0, errors.New("refused"))
	})
}

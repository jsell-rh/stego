package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// MaxRequestBody is the most bytes of a request body that Serve lets Decode read.
const MaxRequestBody = 1 << 20

// CursorParameter is the URL query parameter that holds the text form of a list cursor (ST-SUP-RP11-LC15).
const CursorParameter = "cursor"

// ErrBadRequest is the error that a generated Decode wraps for a request that does not decode or is not valid. The
// helper answers it with 400.
var ErrBadRequest = errors.New("query: the request is not valid")

// HTTP is what the generated code gives Serve for one query: only what varies with the declaration (ST-SUP-RP11-LC17,
// ST-STD-RUNTIME-R05).
type HTTP[Req, Res, P any] struct {
	// Query is the query declaration.
	Query *Query
	// Bind gives the verified tenant and principal of the request, the probe call bound to them and to the query
	// name, the typed probe constructor and the request ID. An error is answered with 401.
	Bind func(r *http.Request) (Bound[P], error)
	// Decode reads the request of the query from the HTTP request; after is the cursor of the cursor parameter, the
	// zero cursor when there is none. An error is answered with 400.
	Decode func(r *http.Request, after Cursor) (Req, error)
	// Fill is the query fill.
	Fill Fill[Req, Res, P]
	// Next gives the cursor of the next page of a list response, or the zero cursor at the end. It is nil for a
	// query that is not a list.
	Next func(res Res) Cursor
	// Encode writes the response body: res, and next, the text form of the next cursor, empty at the end.
	Encode func(res Res, next string) ([]byte, error)
	// Refusal is the refusal lookup, which the generated code sets to queries.Declared: it gives the declared status
	// and code of an error of the fill that is a refusal that the query declares, and false for every other error.
	// Nil knows no refusal (ST-SUP-RP11-LC17).
	Refusal func(err error) (status int, code string, ok bool)
}

// ErrorCodeInternal is the fixed code of a query that failed with an error that is not a declared refusal.
const ErrorCodeInternal = "internal"

// problem is a problem response (RFC 9457). It holds a fixed title, the status and, for a declared refusal or a
// failed query, the fixed code only: no error text and no data of a row.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code,omitempty"`
}

// The fixed titles of the problem responses.
var titles = map[int]string{
	http.StatusBadRequest:          "the request is not valid",
	http.StatusUnauthorized:        "the request is not authenticated",
	http.StatusNotFound:            "the resource is not found",
	http.StatusMethodNotAllowed:    "the method is not allowed",
	http.StatusInternalServerError: "the query failed",
	http.StatusServiceUnavailable:  "the read could not be decided",
}

// Serve returns the HTTP handler of the query that s declares (ST-SUP-RP11-LC17): for each GET request it binds the
// verified identity, reads the cursor parameter with ParseCursor, before any transaction and so with no row read,
// decodes the request, whose body it limits to MaxRequestBody, runs the query with Run, and writes the response as
// JSON with 200 and Cache-Control: no-store, as it does each problem response. It maps each refusal to a
// problem response: a bad cursor or request to 400, a failed bind to 401, a row that is not there or is denied to
// 404, a read that could not be decided, also a list whose provider fails, to 503, and every other error to 500. A problem response holds no error
// text and no data of a row. It refuses a list declaration when the runtime has no cursor key.
func Serve[Req, Res, P any](rt *Runtime, s HTTP[Req, Res, P]) (http.Handler, error) {
	switch {
	case rt == nil:
		return nil, errors.New("query: no runtime")
	case s.Query == nil || s.Bind == nil || s.Decode == nil || s.Fill == nil || s.Encode == nil:
		return nil, errors.New("query: Serve needs the query, Bind, Decode, Fill and Encode")
	case s.Next != nil && rt.cursorAEAD == nil:
		return nil, ErrNoCursorKey
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A query response holds data of one principal: no cache keeps it.
		w.Header().Set("Cache-Control", "no-store")
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBody)
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeProblem(w, http.StatusMethodNotAllowed)
			return
		}
		b, err := s.Bind(r)
		if err != nil {
			writeProblem(w, http.StatusUnauthorized)
			return
		}
		// The probe call ends here when no Run takes it.
		ran := false
		defer func() {
			if !ran && b.Call != nil {
				b.Call.Returned()
				b.Call.Rollback()
			}
		}()
		var after Cursor
		if s.Next != nil {
			if after, err = rt.ParseCursor(s.Query, b.Tenant, b.Principal, r.URL.Query().Get(CursorParameter)); err != nil {
				writeProblem(w, http.StatusBadRequest)
				return
			}
		}
		req, err := s.Decode(r, after)
		if err != nil {
			writeProblem(w, http.StatusBadRequest)
			return
		}
		ran = true
		res, err := Run(r.Context(), rt, s.Query, b, req, s.Fill)
		if err != nil {
			status, code := statusOf(err), ""
			if s.Refusal != nil {
				if declared, declaredCode, ok := s.Refusal(err); ok && declared >= 400 && declared <= 599 {
					status, code = declared, declaredCode
				}
			}
			if code == "" && status == http.StatusInternalServerError {
				code = ErrorCodeInternal
			}
			// The completion log of the request holds the fixed code, never the error text (ST-SUP-RP03-LC12).
			otel.SetErrorCode(r.Context(), code)
			writeCodedProblem(w, status, code)
			return
		}
		next := ""
		if s.Next != nil {
			if next, err = rt.CursorText(s.Query, b.Tenant, b.Principal, s.Next(res)); err != nil {
				writeProblem(w, http.StatusInternalServerError)
				return
			}
		}
		body, err := s.Encode(res, next)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}), nil
}

// statusOf is the status of an error of Run.
func statusOf(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrBadCursor), errors.Is(err, ErrBadRequest):
		return http.StatusBadRequest
	case errors.Is(err, ErrUndecided):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// writeProblem writes the problem response of status.
func writeProblem(w http.ResponseWriter, status int) { writeCodedProblem(w, status, "") }

// writeCodedProblem writes the problem response of status with the fixed code, when there is one.
func writeCodedProblem(w http.ResponseWriter, status int, code string) {
	title := titles[status]
	if title == "" {
		title = http.StatusText(status)
	}
	body, _ := json.Marshal(problem{Type: "about:blank", Title: title, Status: status, Code: code})
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Refusal is one refusal that a query declares (ST-SUP-RC33-OC07): the query, its name, its HTTP status and its
// fixed code. Its fields are unexported: the generated contract makes each declared value once with NewRefusal, and
// its lookup knows only those values, so a refusal that a fill makes itself is answered 500 internal. Its text is
// its code alone, never a text of the fill.
type Refusal struct {
	query, name, code string
	status            int
}

// NewRefusal makes the refusal name of the query named query, with its status and its fixed code. The generated
// contract calls it once for each declared refusal.
func NewRefusal(query, name string, status int, code string) *Refusal {
	return &Refusal{query: query, name: name, status: status, code: code}
}

// Error is the refusal's code.
func (r *Refusal) Error() string {
	if r == nil {
		return "refused"
	}
	return r.code
}

// Query is the query that declares the refusal, Name its declared name, Status its HTTP status and Code its fixed
// code.
func (r *Refusal) Query() string { return r.query }
func (r *Refusal) Name() string  { return r.name }
func (r *Refusal) Status() int   { return r.status }
func (r *Refusal) Code() string  { return r.code }

// Lookup gives the refusal lookup of HTTP for the declared refusals of one query: it knows an error that is one of
// declared, by identity, and no other error.
func Lookup(declared ...*Refusal) func(error) (int, string, bool) {
	return func(err error) (int, string, bool) {
		var r *Refusal
		if !errors.As(err, &r) {
			return 0, "", false
		}
		for _, d := range declared {
			if d != nil && d == r {
				return r.status, r.code, true
			}
		}
		return 0, "", false
	}
}

// Probes binds the probe call of one query call to its span context, tenant, principal and query name: the probe
// runtime, or its Recorder in a test.
type Probes interface {
	Bind(ctx context.Context, tenant, principal, operation string) *probe.Call
}

// NewRequestID gives a new request ID for the audit rows of one query call: a random UUID that the server makes, as
// the command runtime makes one for a command (ST-SUP-RP11-LC19). No value of the client is the request ID.
func NewRequestID() string { return uuid.NewString() }

// BindContext gives the bound call of the query q for the verified identity of ctx (ST-SUP-RP11-LC17): the tenant of
// its verified claim, the principal of subjectType that its subject maps to, and the probe call of probes bound to
// both and to the query name, with newProbe as the typed probe constructor. requestID is a request ID that the
// server made for the call, such as NewRequestID gives, never a value of the client; empty gets a new one. A
// context with no verified identity, or one whose claim gives no tenant, is not bound: an error that Serve answers
// with 401.
func BindContext[P any](ctx context.Context, probes Probes, subjectType string, q *Query, newProbe func(*probe.Probe) P,
	requestID string) (Bound[P], error) {
	if probes == nil || q == nil || newProbe == nil || subjectType == "" {
		return Bound[P]{}, errors.New("query: a bind needs the probes, the subject type, the query and its probe")
	}
	id, ok := authn.FromContext(ctx)
	if !ok {
		return Bound[P]{}, errors.New("query: the request holds no verified identity")
	}
	t, err := tenant.New(tenant.Claim(id))
	if err != nil {
		return Bound[P]{}, err
	}
	p, err := authz.Person(subjectType, id)
	if err != nil {
		return Bound[P]{}, err
	}
	if requestID == "" {
		requestID = NewRequestID()
	}
	call := probes.Bind(ctx, t.ID(), p.Subject().String(), q.Name())
	return Bound[P]{Tenant: t, Principal: p, Call: call, Probe: newProbe, RequestID: requestID}, nil
}

// BindHTTP gives the Bind of HTTP for the query q: BindContext of the request's context, with a new request ID that
// the server makes. A request ID header of the client, such as X-Request-Id, is not read, so it never reaches an
// audit row (ST-SUP-RP11-LC19).
func BindHTTP[P any](probes Probes, subjectType string, q *Query, newProbe func(*probe.Probe) P) func(r *http.Request) (Bound[P], error) {
	return func(r *http.Request) (Bound[P], error) {
		return BindContext(r.Context(), probes, subjectType, q, newProbe, NewRequestID())
	}
}

// LimitParameter is the URL query parameter that holds the most values of a list page.
const LimitParameter = "limit"

// Limit reads the limit parameter of a list request (ST-SUP-RP11-LC17): absent is 0, the default page; a value
// that is not a whole number from 1 to MaxLimit is ErrBadRequest, which Serve answers with 400.
func Limit(r *http.Request) (int, error) {
	text := r.URL.Query().Get(LimitParameter)
	if text == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(text)
	if err != nil || limit < 1 || limit > MaxLimit {
		return 0, ErrBadRequest
	}
	return limit, nil
}

// page is the body of a list response: its items and the sealed text of the next cursor, absent at the end. It has
// no total (ST-SUP-RP11-LC18).
type page struct {
	Items any    `json:"items"`
	Next  string `json:"next,omitempty"`
}

// EncodePage writes the body of a list response as JSON: the items and next, the text of the next cursor, which is
// empty at the end of the list (ST-SUP-RP11-LC17). It is the Encode of a list query.
func EncodePage(items any, next string) ([]byte, error) {
	return json.Marshal(page{Items: items, Next: next})
}

// EncodeValue writes the response of a get query as JSON. It is the Encode of a get query.
func EncodeValue[Res any](res Res, _ string) ([]byte, error) { return json.Marshal(res) }

// RevocationUnknown is the state that AwaitWrite answers for a revocation that is not one of the caller's own: it
// does not tell whether another tenant or principal made one with that identity.
const RevocationUnknown = "unknown"

// AwaitWrite waits for the revocation of the caller's own write that the request names in Authz-Revocation-After,
// before the read opens its transaction (ST-SUP-RP11-LC21). The value is the identity of the revocation or the
// path of Authz-Revocation-Location. It waits through authz WaitFor, at most the wait of Prefer: wait=N when the
// request gives one and at most the wait cap, and holds no transaction and no pooled connection while it waits.
// It sets the response header Authz-Revocation to the state it read last, and to unknown for a revocation that is
// not one of tenant t and principal p. Once that state is complete, the read that follows runs at least as fresh as
// the revocation's token, because the revocation floor that each check and list reads was raised before the
// revocation became complete (ST-SUP-RP06-LC13, ST-SUP-RP06-LC15). A request with no such header waits for
// nothing. A value that is not an identity is ErrBadRequest.
func (rt *Runtime) AwaitWrite(w http.ResponseWriter, r *http.Request, t tenant.Tenant, p authz.Principal) error {
	text := r.Header.Get(authz.RevocationAfterHeader)
	if text == "" {
		return nil
	}
	if len(r.Header.Values(authz.RevocationAfterHeader)) != 1 {
		return fmt.Errorf("%w: more than one %s header", ErrBadRequest, authz.RevocationAfterHeader)
	}
	text = strings.TrimPrefix(text, authz.RevocationPath)
	id, err := uuid.Parse(text)
	if err != nil || id.String() != text {
		return fmt.Errorf("%w: %s is not the identity of a revocation", ErrBadRequest, authz.RevocationAfterHeader)
	}
	if rt.authz == nil {
		return errors.New("query: no authorization runtime to wait for a revocation")
	}
	wait, ok := authz.PreferWait(r.Header)
	if !ok {
		wait = math.MaxInt64
	}
	status, _, err := rt.authz.WaitFor(r.Context(), t, p, id, wait)
	switch {
	case errors.Is(err, authz.ErrRevocationNotFound):
		w.Header().Set(authz.RevocationHeader, RevocationUnknown)
		return nil
	case err != nil:
		return err
	}
	w.Header().Set(authz.RevocationHeader, string(status.State))
	return nil
}

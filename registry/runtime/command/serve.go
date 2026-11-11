package command

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// MaxRequestBody is the most bytes of a request body that Serve reads.
const MaxRequestBody = 1 << 20

// RPCKeyMetadata is the gRPC request metadata that holds the idempotency key (ST-SUP-RC07-OC19).
const RPCKeyMetadata = "idempotency-key"

// ErrorCodeDenied is the fixed code of a command that the principal may not run, or whose resource is missing.
const ErrorCodeDenied = "denied"

// ErrBadRequest is the error that a generated Path wraps for a request that does not decode into the request
// message or whose path parameter differs from its body. Serve answers it with 400.
var ErrBadRequest = errors.New("command: the request does not decode into the request message")

// OutcomeTimeout bounds the write of the audit row of a denied or failed command.
const OutcomeTimeout = 5 * time.Second

// Refusal is one refusal that a command declares (ST-SUP-RC32-OC09): the command, its name, its HTTP status and its
// fixed code. Its fields are unexported: the generated contract makes each declared value once with NewRefusal, and
// Serve knows only the values of the spec, so a refusal that a fill makes itself is answered 500 internal.
type Refusal struct {
	command, name, code string
	status              int
}

// NewRefusal makes the refusal name of the command named command, with its HTTP status and its fixed code. The
// generated contract calls it once for each declared refusal.
func NewRefusal(command, name string, status int, code string) *Refusal {
	return &Refusal{command: command, name: name, status: status, code: code}
}

// Error names the command and the refusal, and no value of a request or a row.
func (r *Refusal) Error() string { return "command " + r.command + ": refused: " + r.name }

// Is makes a refusal a declared refusal, which RunConsumer parks (ST-SUP-RP10-LC26).
func (r *Refusal) Is(target error) bool { return target == errDeclaredRefusal }

// Command is the command that declares the refusal, Name its declared name, Status its HTTP status and Code its
// fixed code.
func (r *Refusal) Command() string { return r.command }
func (r *Refusal) Name() string    { return r.name }
func (r *Refusal) Status() int     { return r.status }
func (r *Refusal) Code() string    { return r.code }

// ServeSpec is what varies with the declaration of one command, which the generated code gives Serve and ServeRPC
// (ST-SUP-RP10-LC33, ST-STD-RUNTIME-R05).
type ServeSpec[Req, Res proto.Message, P any] struct {
	// Command is the checked command declaration, and ResourceType the resource type of its audit rows.
	Command      *Command[P]
	ResourceType string
	// SubjectType is the subject type of the schema that a verified identity maps to, such as user.
	SubjectType string
	// Status is the HTTP status of a committed command, such as 201 for a create.
	Status int
	// NewRequest and NewResponse give an empty request and response message.
	NewRequest  func() Req
	NewResponse func() Res
	// Path sets the request fields of the path parameters of r; nil for a command with none. A field that the body
	// sets to another value is ErrBadRequest.
	Path func(r *http.Request, request Req) error
	// Resource gives the ID of the resource that the request names, on which Run decides the permission.
	Resource func(request Req) string
	// Fill is the one fill of the command.
	Fill func(ctx context.Context, request Req, h *Handle[P]) (Res, error)
	// Refusals are the declared refusals of the command.
	Refusals []*Refusal
	// KeyRequired is true when the declaration requires the Idempotency-Key header; without it, a request with no
	// key runs without the idempotency wrapper (ST-SUP-RP10-LC11).
	KeyRequired bool
	// Retention is how long a completed idempotency key is kept; zero is the runtime's default.
	Retention time.Duration
}

func (s ServeSpec[Req, Res, P]) check(rt *Runtime) error {
	if rt == nil || s.Command == nil || s.Fill == nil || s.NewRequest == nil || s.NewResponse == nil ||
		s.Resource == nil || s.SubjectType == "" || s.Status < 200 || s.Status > 299 {
		return errors.New("command: Serve needs the runtime, the command, the subject type, a 2xx status, the messages, the resource and the fill")
	}
	return nil
}

// Outcome is how a command handler answers an error of Run (ST-SUP-RP10-LC34): the HTTP status, the fixed code and
// the audit outcome. It holds no error text.
type Outcome struct {
	Status int
	Code   string
	Audit  audit.Outcome
}

// OutcomeOf maps an error of Run to its answer (ST-SUP-RP10-LC33, ST-SUP-RP10-LC34): a declared refusal among
// refusals is its status and code; ErrDenied is 404 denied, the same answer as a missing row of a read
// (ST-SUP-RP11-LC06); a conflict after the last attempt is 409 conflict; a COMMIT whose outcome is not known and
// every other error are 500 internal, the first with the audit code commit_outcome_unknown. A refusal that is not
// one of refusals is an other error. No error text reaches the answer, the audit row or the log. Serve uses it, and
// so does a handler that decodes its own body, such as the handlers of generated CRUD.
func OutcomeOf(err error, refusals ...*Refusal) Outcome {
	var refused *Refusal
	if errors.As(err, &refused) {
		for _, r := range refusals {
			if r != nil && r == refused {
				return Outcome{Status: r.status, Code: r.code, Audit: audit.OutcomeFailed}
			}
		}
	}
	if errors.Is(err, ErrDenied) {
		return Outcome{Status: http.StatusNotFound, Code: ErrorCodeDenied, Audit: audit.OutcomeDenied}
	}
	code := ErrorCode(err)
	if code == audit.ErrorCodeConflict {
		return Outcome{Status: http.StatusConflict, Code: code, Audit: audit.OutcomeFailed}
	}
	return Outcome{Status: http.StatusInternalServerError, Code: code, Audit: audit.OutcomeFailed}
}

// AnswerCode is the code of the answer to the client: the audit code commit_outcome_unknown is internal to a client.
func (o Outcome) AnswerCode() string {
	if o.Code == audit.ErrorCodeCommitOutcomeUnknown {
		return ErrorCodeInternal
	}
	return o.Code
}

// Bound is the verified tenant and principal of a request.
type Bound struct {
	Tenant    tenant.Tenant
	Principal authz.Principal
}

// BindIdentity takes the tenant from the verified claim of ctx and maps the verified identity to its principal of
// subjectType (ST-SUP-RC32-OC04, ST-SUP-RP10-LC34). A context with no verified identity, or one with no tenant
// claim, is not bound. A handler binds before it opens any transaction, and answers 401 when it cannot.
func BindIdentity(ctx context.Context, subjectType string) (Bound, error) {
	identity, ok := authn.FromContext(ctx)
	if !ok {
		return Bound{}, errors.New("command: no verified identity")
	}
	t, err := tenant.New(tenant.Claim(identity))
	if err != nil {
		return Bound{}, err
	}
	p, err := authz.Person(subjectType, identity)
	if err != nil {
		return Bound{}, err
	}
	return Bound{Tenant: t, Principal: p}, nil
}

// RecordFailure writes the outcome audit row of a request whose Run ended in an error, with the action, the resource
// type, the request's resource and the outcome and fixed code of o (ST-SUP-RC32-OC07, ST-SUP-RP10-LC34). It runs in
// its own short transaction within OutcomeTimeout, which the end of ctx does not cancel, so the row is written also
// when the request was cancelled or ran past its deadline. It gives the fixed code to the completion log of ctx
// (ST-SUP-RP03-LC12).
func (rt *Runtime) RecordFailure(ctx context.Context, req Request, action, resourceType string, o Outcome) error {
	otel.SetErrorCode(ctx, o.AnswerCode())
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), OutcomeTimeout)
	defer cancel()
	return rt.RecordOutcome(recordCtx, req, action, resourceType, req.Resource, o.Audit, o.Code)
}

// serveRun runs the command in Run and, when it ends in an error, writes the command's audit row in its own short
// transaction with the outcome denied or failed and its fixed code, and gives the code to the completion log.
func serveRun[Req, Res proto.Message, P any](ctx context.Context, rt *Runtime, s ServeSpec[Req, Res, P], req Request,
	decode func([]byte) (Req, error)) (Result, *Outcome) {
	result, err := Run(ctx, rt, s.Command, req, decode, func(ctx context.Context, request Req, h *Handle[P]) (idempotency.Response, error) {
		response, err := s.Fill(ctx, request, h)
		if err != nil {
			return idempotency.Response{}, err
		}
		if !response.ProtoReflect().IsValid() {
			return idempotency.Response{}, errors.New("the fill returned no response message")
		}
		body, err := protojson.Marshal(response)
		if err != nil {
			return idempotency.Response{}, err
		}
		return idempotency.Response{Status: s.Status, Header: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
	})
	if err == nil {
		return result, nil
	}
	o := OutcomeOf(err, s.Refusals...)
	_ = rt.RecordFailure(ctx, req, s.Command.spec.Name, s.ResourceType, o)
	return result, &o
}

// Serve gives the HTTP handler of one command (ST-SUP-RP10-LC33). It binds the verified tenant and principal before
// any transaction (401 when it cannot), reads the Idempotency-Key header (400 when it is required and missing or
// malformed), decodes the body with protojson and the path parameters into the request message (400), and runs the
// one fill through Run, which decides the permission before the fill. It answers ErrDenied with 404, a declared
// refusal with its status and code, ErrConflict with 409 conflict, and ErrCommitUnknown and any other error with 500
// internal; it writes the outcome audit row of a denied or failed command, and a problem response with a fixed
// title, the status and the fixed code only.
func Serve[Req, Res proto.Message, P any](rt *Runtime, s ServeSpec[Req, Res, P]) (http.Handler, error) {
	if err := s.check(rt); err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch && r.Method != http.MethodDelete {
			w.Header().Set("Allow", "POST, PUT, PATCH, DELETE")
			writeProblem(w, http.StatusMethodNotAllowed, "", "")
			return
		}
		b, err := BindIdentity(r.Context(), s.SubjectType)
		if err != nil {
			writeProblem(w, http.StatusUnauthorized, "", "")
			return
		}
		key, err := idempotency.KeyFrom(r.Header, s.KeyRequired)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, idempotency.ProblemMissing, "")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBody))
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "", "")
			return
		}
		decode := func(body []byte) (Req, error) {
			request := s.NewRequest()
			if len(body) > 0 {
				if err := (protojson.UnmarshalOptions{}).Unmarshal(body, request); err != nil {
					return request, ErrBadRequest
				}
			}
			if s.Path != nil {
				if err := s.Path(r, request); err != nil {
					return request, ErrBadRequest
				}
			}
			return request, nil
		}
		request, err := decode(body)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "", "")
			return
		}
		fingerprint, err := idempotency.Fingerprint(r.Method, r.URL.Path, body)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "", "")
			return
		}
		req := Request{Tenant: b.Tenant, Principal: b.Principal, Key: key, Fingerprint: fingerprint, Body: body,
			Resource: s.Resource(request), RequestID: NewID(), ActorIP: remoteIP(r.RemoteAddr),
			UserAgent: r.UserAgent(), Retention: s.Retention}
		result, failed := serveRun(r.Context(), rt, s, req, decode)
		switch {
		case failed != nil:
			writeProblem(w, failed.Status, "", failed.AnswerCode())
		case result.Problem != "":
			writeProblem(w, result.Response.Status, result.Problem, "")
		default:
			for name, values := range result.Response.Header {
				w.Header()[name] = values
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(result.Response.Body)))
			w.WriteHeader(result.Response.Status)
			_, _ = w.Write(result.Response.Body)
		}
	}), nil
}

// ServeRPC gives the gRPC handler of one command (ST-SUP-RP10-LC33): the same bind, idempotency, Run, outcomes and
// audit as Serve, with the idempotency key from the RPCKeyMetadata request metadata, and the answer as a gRPC status
// whose message is the fixed code alone. The binding needs a context that holds the verified identity, as
// authn.Authenticate gives it.
func ServeRPC[Req, Res proto.Message, P any](rt *Runtime, s ServeSpec[Req, Res, P]) (func(context.Context, Req) (Res, error), error) {
	if err := s.check(rt); err != nil {
		return nil, err
	}
	return func(ctx context.Context, request Req) (Res, error) {
		var none Res
		b, err := BindIdentity(ctx, s.SubjectType)
		if err != nil {
			return none, status.Error(codes.Unauthenticated, "unauthenticated")
		}
		md, _ := metadata.FromIncomingContext(ctx)
		key, err := idempotency.KeyFrom(http.Header{idempotency.KeyHeader: md.Get(RPCKeyMetadata)}, s.KeyRequired)
		if err != nil {
			return none, status.Error(codes.InvalidArgument, idempotency.ProblemMissing)
		}
		// The body that Run decodes is the deterministic binary form of the message, and the fingerprint is over the
		// canonical JSON form of protojson, which Fingerprint makes, so one message gives one fingerprint.
		body, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
		if err != nil {
			return none, status.Error(codes.InvalidArgument, "bad_request")
		}
		text, err := protojson.Marshal(request)
		if err != nil {
			return none, status.Error(codes.InvalidArgument, "bad_request")
		}
		fingerprint, err := idempotency.Fingerprint("RPC", s.Command.spec.Name, text)
		if err != nil {
			return none, status.Error(codes.InvalidArgument, "bad_request")
		}
		decode := func(body []byte) (Req, error) {
			request := s.NewRequest()
			if err := proto.Unmarshal(body, request); err != nil {
				return request, ErrBadRequest
			}
			return request, nil
		}
		req := Request{Tenant: b.Tenant, Principal: b.Principal, Key: key, Fingerprint: fingerprint, Body: body,
			Resource: s.Resource(request), RequestID: NewID(), Retention: s.Retention}
		result, failed := serveRun(ctx, rt, s, req, decode)
		switch {
		case failed != nil:
			return none, status.Error(RPCCode(failed.Status), failed.AnswerCode())
		case result.Problem == idempotency.ProblemInProgress:
			return none, status.Error(codes.Aborted, result.Problem)
		case result.Problem != "":
			return none, status.Error(codes.FailedPrecondition, result.Problem)
		}
		response := s.NewResponse()
		if err := protojson.Unmarshal(result.Response.Body, response); err != nil {
			return none, status.Error(codes.Internal, ErrorCodeInternal)
		}
		return response, nil
	}, nil
}

// RPCCode is the gRPC status code of an HTTP status of a command answer.
func RPCCode(httpStatus int) codes.Code {
	switch httpStatus {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusConflict:
		return codes.Aborted
	case http.StatusPreconditionFailed:
		return codes.FailedPrecondition
	}
	return codes.Internal
}

// SetPath sets a request field from its path parameter. An empty parameter, and a body that gives the field
// another value, is ErrBadRequest.
func SetPath(field *string, value string) error {
	if value == "" || (*field != "" && *field != value) {
		return ErrBadRequest
	}
	*field = value
	return nil
}

// The fixed titles of the problem responses.
var titles = map[int]string{
	http.StatusBadRequest:          "the request is not valid",
	http.StatusUnauthorized:        "the request is not authenticated",
	http.StatusForbidden:           "the request is not allowed",
	http.StatusNotFound:            "the resource is not found",
	http.StatusMethodNotAllowed:    "the method is not allowed",
	http.StatusConflict:            "the request conflicts with the state of the resource",
	http.StatusPreconditionFailed:  "a precondition of the request does not hold",
	http.StatusUnprocessableEntity: "the request cannot be carried out",
	http.StatusInternalServerError: "the command failed",
}

// writeProblem writes a problem response (RFC 9457) with a fixed title, the status, and the problem type and the
// fixed code when they are given. It holds no error text and no value of a row.
func writeProblem(w http.ResponseWriter, httpStatus int, problemType, code string) {
	if problemType == "" {
		problemType = "about:blank"
	}
	title := titles[httpStatus]
	if title == "" {
		title = http.StatusText(httpStatus)
	}
	var b strings.Builder
	b.WriteString("{\"type\":" + strconv.Quote(problemType) + ",\"title\":" + strconv.Quote(title) + ",\"status\":" + strconv.Itoa(httpStatus))
	if code != "" {
		b.WriteString(",\"code\":" + strconv.Quote(code))
	}
	b.WriteString("}")
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(httpStatus)
	_, _ = io.WriteString(w, b.String())
}

// remoteIP is the host part of a remote address.
func remoteIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return host
}

// NewID gives a new random UUID in its text form: the key of a new row and the ID of a request.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ValidUUID tells whether id is a UUID in its text form, the key type of every entity.
func ValidUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// Must gives v, and panics on an error: for a declaration that the generator checked, such as one of NewEntity,
// NewCommand or authz.ParseRelation, which does not fail.
func Must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// ClonePointer gives a copy of the value that p points to, or nil.
func ClonePointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// StateOf gives the value that p points to for an audit state, or nil when p is nil.
func StateOf[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// EqualPointer tells whether two pointers point to equal values or are both nil.
func EqualPointer[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

package command

// renderServe writes the serve helpers of the handlers package: the steps of a command handler that are the same
// for every command (ST-SUP-RC32-OC04 to ST-SUP-RC32-OC10). The runtime library has no HTTP helper for a command
// yet, as it has for a query (ST-SUP-RP11-LC17); until it has, the generator writes them once per service.
func renderServe(m *model) []byte {
	w := &writer{}
	w.f("package %s\n\nimport (\n\t\"context\"\n\t\"errors\"\n\t\"io\"\n\t\"net\"\n\t\"net/http\"\n\t\"strconv\"\n\t\"strings\"\n\t\"time\"\n\n", handlersFolder)
	w.f("\t\"google.golang.org/grpc/codes\"\n\t\"google.golang.org/grpc/metadata\"\n\t\"google.golang.org/grpc/status\"\n\t\"google.golang.org/protobuf/encoding/protojson\"\n\t\"google.golang.org/protobuf/proto\"\n\n")
	w.f("\t\"stego.dev/runtime/audit\"\n\t\"stego.dev/runtime/authn\"\n\t\"stego.dev/runtime/authz\"\n\t\"stego.dev/runtime/command\"\n\t\"stego.dev/runtime/idempotency\"\n")
	if m.probeImport == "" {
		w.f("\t\"stego.dev/runtime/probe\"\n")
	}
	w.f("\t\"stego.dev/runtime/tenant\"\n\n\tcommands %q\n)\n\n%s%s", m.contractImport, serveSource, RunHelpers)
	if m.probeImport == "" {
		w.f("\n// noProbe is the typed probe of a service that has no domain probes.\nfunc noProbe(*probe.Probe) struct{} { return struct{}{} }\n")
	}
	return w.Bytes()
}

// serveSource is the body of serve.go after its imports.
const serveSource = `// Options are the settings of the command handlers.
type Options struct {
	// Retention is how long a completed idempotency key is kept; zero is the runtime's default.
	Retention time.Duration
}

// MaxRequestBody is the most bytes of a request body that a command handler reads.
const MaxRequestBody = 1 << 20

// rpcKeyMetadata is the gRPC request metadata that holds the idempotency key (ST-SUP-RC07-OC19).
const rpcKeyMetadata = "idempotency-key"

// errBadRequest is the error of a request that does not decode into the request message.
var errBadRequest = errors.New("the request does not decode into the request message")

// spec is what varies with the declaration of one command.
type spec[Req, Res proto.Message, P any] struct {
	name         string
	command      *command.Command[P]
	resourceType string
	status       int
	newRequest   func() Req
	newResponse  func() Res
	path         func(r *http.Request, request Req) error
	resource     func(request Req) string
	fill         func(ctx context.Context, request Req, h *command.Handle[P]) (Res, error)
	hasFill      bool
	refusals     []*commands.Refusal
	options      Options
}

// outcomeOf maps an error of Run to its answer (ST-SUP-RC32-OC09): a declared refusal of this command is its status
// and code, and every other error is the answer of failedOutcome.
func outcomeOf(err error, refusals []*commands.Refusal) outcome {
	var refused *commands.Refusal
	if errors.As(err, &refused) {
		for _, r := range refusals {
			if r == refused {
				return outcome{status: r.Status(), code: r.Code(), audit: audit.OutcomeFailed}
			}
		}
	}
	return failedOutcome(err)
}

// run runs the command in Run and, when it ends in an error, writes the command's audit row in its own short
// transaction with the outcome denied or failed and its fixed code (ST-SUP-RC32-OC07): denied for a deny.
func run[Req, Res proto.Message, P any](ctx context.Context, rt *command.Runtime, s spec[Req, Res, P], req command.Request,
	decode func([]byte) (Req, error)) (command.Result, *outcome) {
	result, err := command.Run(ctx, rt, s.command, req, decode, func(ctx context.Context, request Req, h *command.Handle[P]) (idempotency.Response, error) {
		response, err := s.fill(ctx, request, h)
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
		return idempotency.Response{Status: s.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
	})
	if err == nil {
		return result, nil
	}
	o := outcomeOf(err, s.refusals)
	recordOutcome(ctx, rt, req, s.name, s.resourceType, o)
	return result, &o
}

// serveHTTP gives the HTTP handler of one command. It verifies the identity and binds the tenant and the principal
// before any transaction (401 when it cannot), requires the Idempotency-Key header (400), decodes the body with
// protojson and the path parameters into the request message (400), and runs the one fill through Run, which
// decides the permission before the fill and runs it at most once per transaction (ST-SUP-RC32-OC04,
// ST-SUP-RC32-OC08). A problem response holds a fixed title, the status and a fixed code only.
func serveHTTP[Req, Res proto.Message, P any](rt *command.Runtime, s spec[Req, Res, P]) (http.Handler, error) {
	if rt == nil || s.command == nil || !s.hasFill {
		return nil, errors.New("command " + s.name + ": the handler needs the runtime and the fill")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeProblem(w, http.StatusMethodNotAllowed, "", "")
			return
		}
		b, err := bind(r.Context())
		if err != nil {
			writeProblem(w, http.StatusUnauthorized, "", "")
			return
		}
		key, err := idempotency.KeyFrom(r.Header, true)
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
			request := s.newRequest()
			if len(body) > 0 {
				if err := (protojson.UnmarshalOptions{}).Unmarshal(body, request); err != nil {
					return request, errBadRequest
				}
			}
			if err := s.path(r, request); err != nil {
				return request, err
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
		req := command.Request{Tenant: b.tenant, Principal: b.principal, Key: key, Fingerprint: fingerprint, Body: body,
			Resource: s.resource(request), RequestID: newRequestID(), ActorIP: remoteIP(r.RemoteAddr),
			UserAgent: r.UserAgent(), Retention: s.options.Retention}
		result, failed := run(r.Context(), rt, s, req, decode)
		switch {
		case failed != nil:
			writeProblem(w, failed.status, "", failed.answerCode())
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

// serveRPC gives the gRPC handler of one command: the same binding, idempotency, Run and audit as its HTTP handler,
// with the idempotency key from the idempotency-key request metadata, and the answer as a gRPC status. A conflict
// after the last attempt is ABORTED (ST-SUP-RC32-OC10).
func serveRPC[Req, Res proto.Message, P any](rt *command.Runtime, s spec[Req, Res, P]) (func(context.Context, Req) (Res, error), error) {
	if rt == nil || s.command == nil || !s.hasFill {
		return nil, errors.New("command " + s.name + ": the handler needs the runtime and the fill")
	}
	return func(ctx context.Context, request Req) (Res, error) {
		var none Res
		b, err := bind(ctx)
		if err != nil {
			return none, status.Error(codes.Unauthenticated, "the call is not authenticated")
		}
		md, _ := metadata.FromIncomingContext(ctx)
		key, err := idempotency.KeyFrom(http.Header{idempotency.KeyHeader: md.Get(rpcKeyMetadata)}, true)
		if err != nil {
			return none, status.Error(codes.InvalidArgument, idempotency.ProblemMissing)
		}
		// The body that Run decodes is the deterministic binary form of the message, and the fingerprint is over the
		// canonical JSON form of protojson, which Fingerprint makes, so one message gives one fingerprint.
		body, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
		if err != nil {
			return none, status.Error(codes.InvalidArgument, "the request message does not encode")
		}
		text, err := protojson.Marshal(request)
		if err != nil {
			return none, status.Error(codes.InvalidArgument, "the request message does not encode")
		}
		fingerprint, err := idempotency.Fingerprint("RPC", s.name, text)
		if err != nil {
			return none, status.Error(codes.InvalidArgument, "the request message does not encode")
		}
		decode := func(body []byte) (Req, error) {
			request := s.newRequest()
			if err := proto.Unmarshal(body, request); err != nil {
				return request, errBadRequest
			}
			return request, nil
		}
		req := command.Request{Tenant: b.tenant, Principal: b.principal, Key: key, Fingerprint: fingerprint, Body: body,
			Resource: s.resource(request), RequestID: newRequestID(), Retention: s.options.Retention}
		result, failed := run(ctx, rt, s, req, decode)
		switch {
		case failed != nil:
			return none, status.Error(rpcCode(failed.status), failed.answerCode())
		case result.Problem == idempotency.ProblemInProgress:
			return none, status.Error(codes.Aborted, result.Problem)
		case result.Problem != "":
			return none, status.Error(codes.FailedPrecondition, result.Problem)
		}
		response := s.newResponse()
		if err := protojson.Unmarshal(result.Response.Body, response); err != nil {
			return none, status.Error(codes.Internal, command.ErrorCodeInternal)
		}
		return response, nil
	}, nil
}

// rpcCode is the gRPC status code of an HTTP status of a command answer.
func rpcCode(httpStatus int) codes.Code {
	switch httpStatus {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return codes.InvalidArgument
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

// setPath sets a request field from its path parameter. A body that gives the field another value is not a valid
// request.
func setPath(field *string, value string) error {
	if value == "" || (*field != "" && *field != value) {
		return errBadRequest
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

// newRequestID gives the ID of one request, which its audit rows carry.
func newRequestID() string { return commands.NewID() }

// mustCommand gives a checked command declaration. The generator checked each declaration, so it does not fail.
func mustCommand[P any](c *command.Command[P], err error) *command.Command[P] {
	if err != nil {
		panic(err)
	}
	return c
}

// mustRelation gives a checked relation.
func mustRelation(text string) authz.Relation {
	r, err := authz.ParseRelation(text)
	if err != nil {
		panic(err)
	}
	return r
}
`

// RunHelpers are the steps of a command handler around Run that do not depend on how the request is decoded: the
// binding of the verified tenant and principal (ST-SUP-RC32-OC04), the map of an error of Run to its status, its fixed
// code and its audit outcome (ST-SUP-RC32-OC06, ST-SUP-RC32-OC10), and the audit row of a denied or failed command in
// its own short transaction (ST-SUP-RC32-OC07). The handlers package of this generator holds them, and so does the
// package of generated CRUD on the command runtime (ST-SUP-RC20-OC37), so they have one source. The package that
// holds them declares the constant subjectType and imports context, errors, net/http, time and the runtime packages
// audit, authn, authz, command and tenant. ST-SUP-Q74 asks whether they move into the runtime library.
const RunHelpers = `// outcomeTimeout bounds the write of the audit row of a denied or failed command.
const outcomeTimeout = 5 * time.Second

// bound is the verified tenant and principal of a request.
type bound struct {
	tenant    tenant.Tenant
	principal authz.Principal
}

// bind takes the tenant from the verified claim and maps the verified identity to its principal
// (ST-SUP-RC32-OC04). A context with no verified identity, or one with no tenant claim, is not bound.
func bind(ctx context.Context) (bound, error) {
	identity, ok := authn.FromContext(ctx)
	if !ok {
		return bound{}, errors.New("no verified identity")
	}
	t, err := tenant.New(tenant.Claim(identity))
	if err != nil {
		return bound{}, err
	}
	p, err := authz.Person(subjectType, identity)
	if err != nil {
		return bound{}, err
	}
	return bound{tenant: t, principal: p}, nil
}

// outcome is how a handler answers an error of Run: the status, the fixed code and the audit outcome.
type outcome struct {
	status int
	code   string
	audit  audit.Outcome
}

// failedOutcome maps an error of Run that is not a declared refusal to its answer (ST-SUP-RC32-OC06,
// ST-SUP-RC32-OC10): a deny is 403 and denied; a conflict after the last attempt is 409 conflict; a COMMIT whose
// outcome is not known and every other error are 500 internal, the first with the audit code
// commit_outcome_unknown. No error text reaches the answer or the audit row.
func failedOutcome(err error) outcome {
	if errors.Is(err, command.ErrDenied) {
		return outcome{status: http.StatusForbidden, code: "denied", audit: audit.OutcomeDenied}
	}
	code := command.ErrorCode(err)
	if code == audit.ErrorCodeConflict {
		return outcome{status: http.StatusConflict, code: code, audit: audit.OutcomeFailed}
	}
	return outcome{status: http.StatusInternalServerError, code: code, audit: audit.OutcomeFailed}
}

// answerCode is the code of an answer: the audit code commit_outcome_unknown is internal to a client.
func (o outcome) answerCode() string {
	if o.code == audit.ErrorCodeCommitOutcomeUnknown {
		return command.ErrorCodeInternal
	}
	return o.code
}

// recordOutcome writes the audit row of a denied or failed command with its action and resource type in its own
// short transaction (ST-SUP-RC32-OC07). It is written also when the request was cancelled or ran past its
// deadline, within its own short time.
func recordOutcome(ctx context.Context, rt *command.Runtime, req command.Request, action, resourceType string, o outcome) {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeTimeout)
	defer cancel()
	_ = rt.RecordOutcome(recordCtx, req, action, resourceType, req.Resource, o.audit, o.code)
}
`

package command

// RunHelpers are the steps of a command handler around Run that do not depend on how the request is decoded: the
// binding of the verified tenant and principal (ST-SUP-RC32-OC04), the map of an error of Run to its status, its fixed
// code and its audit outcome (ST-SUP-RC32-OC06, ST-SUP-RC32-OC10), and the audit row of a denied or failed command in
// its own short transaction (ST-SUP-RC32-OC07). The handlers package of this generator holds them, and so does the
// package of generated CRUD on the command runtime (ST-SUP-RC20-OC37), so they have one source. The package that
// holds them declares the constant subjectType and imports context, errors, net/http, time and the runtime packages
// audit, authn, authz, command and tenant. The command handlers of this generator use command.Serve of the runtime
// library instead (ST-SUP-RP10-LC33); generated CRUD keeps these steps, because its handlers decode JSON and run the
// bound slots around Run, which command.Serve does not (ST-SUP-Q96).
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
// ST-SUP-RC32-OC10): a deny is 404 and denied, the same answer as a missing row (ST-SUP-RP10-LC31); a conflict after the last attempt is 409 conflict; a COMMIT whose
// outcome is not known and every other error are 500 internal, the first with the audit code
// commit_outcome_unknown. No error text reaches the answer or the audit row.
func failedOutcome(err error) outcome {
	if errors.Is(err, command.ErrDenied) {
		return outcome{status: http.StatusNotFound, code: "denied", audit: audit.OutcomeDenied}
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

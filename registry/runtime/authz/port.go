package authz

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Resource is the resource of an evaluation, in the shape of OpenID AuthZEN 1.0: a type and an ID.
type Resource struct {
	Type string
	ID   string
}

// String returns the resource as type:id, which is also the resource key of its tuples in the outbox.
func (r Resource) String() string { return r.Type + ":" + r.ID }

func (r Resource) valid() error {
	if err := validType("resource", r.Type); err != nil {
		return err
	}
	return validID("resource", r.ID)
}

// Action is the action of an evaluation, in the shape of OpenID AuthZEN 1.0: its name is the permission to check.
type Action struct {
	Name string
}

// Token is the opaque authorization token of a provider: the state at which a provider wrote or checked
// (ST-SUP-RP06-LC04). It has no exported field, and the package never parses, orders or compares two tokens: the
// revision of some SpiceDB datastores, such as the PostgreSQL datastore, is a snapshot with only a partial order,
// and only the provider may judge how fresh a token is. Text gives its text column form, which every protected row
// stores in authz_token, and ParseToken reads it back. The zero value is no token.
type Token struct {
	value string
}

// maxTokenLength is the longest token text the package accepts, in bytes.
const maxTokenLength = 4096

// NewToken builds the token of a provider from the provider's own token value, which it keeps byte for byte. A
// provider calls it.
func NewToken(value string) (Token, error) {
	if value == "" {
		return Token{}, errors.New("authz: a token needs a value")
	}
	if len(value) > maxTokenLength || !cleanText(value) {
		return Token{}, errors.New("authz: the token is too long, holds a control character or is not valid UTF-8")
	}
	return Token{value: value}, nil
}

// ParseToken reads the text column form of a token, as Text writes it. An empty text is the zero token, the value
// of a row that the relay has not written yet.
func ParseToken(text string) (Token, error) {
	if text == "" {
		return Token{}, nil
	}
	return NewToken(text)
}

// Text returns the text column form of the token, or "" for the zero token.
func (t Token) Text() string { return t.value }

// Value returns the provider's own token value, for the provider that made it.
func (t Token) Value() string { return t.value }

// IsZero reports whether t is no token.
func (t Token) IsZero() bool { return t.value == "" }

// consistencyKind is the kind of a Consistency. Its zero value is no consistency, which every evaluation refuses.
type consistencyKind uint8

const (
	consistencyNone consistencyKind = iota
	consistencyFresh
	consistencyAtLeast
)

// Consistency is how fresh an evaluation must be (ST-SUP-RP06-LC03): Fresh, or AtLeast a token. There is no
// minimize-latency option and no exact-snapshot option. The zero value is no consistency, and every evaluation
// refuses it before the provider is called.
type Consistency struct {
	kind  consistencyKind
	token Token
}

// Fresh is the consistency of a check at the newest state of the provider.
func Fresh() Consistency { return Consistency{kind: consistencyFresh} }

// AtLeast is the consistency of a check at least as fresh as token. A zero token gives the zero consistency, which
// an evaluation refuses.
func AtLeast(token Token) Consistency {
	if token.IsZero() {
		return Consistency{}
	}
	return Consistency{kind: consistencyAtLeast, token: token}
}

// IsZero reports whether c is the zero value, which is no consistency.
func (c Consistency) IsZero() bool { return c.kind == consistencyNone }

// IsFresh reports whether c is Fresh.
func (c Consistency) IsFresh() bool { return c.kind == consistencyFresh }

// Token returns the token of an AtLeast consistency, and false for any other.
func (c Consistency) Token() (Token, bool) {
	if c.kind != consistencyAtLeast {
		return Token{}, false
	}
	return c.token, true
}

// String names the kind of the consistency, for errors and logs. It holds no token.
func (c Consistency) String() string {
	switch c.kind {
	case consistencyFresh:
		return "fresh"
	case consistencyAtLeast:
		return "at_least"
	default:
		return "none"
	}
}

// atFloor is the consistency of a check of the handle (ST-SUP-RP06-LC09): at least as fresh as the floor; only while
// the floor is zero, at least as fresh as the row token; and fresh when the row has no token either.
func atFloor(floor, row Token) Consistency {
	switch {
	case !floor.IsZero():
		return AtLeast(floor)
	case !row.IsZero():
		return AtLeast(row)
	default:
		return Fresh()
	}
}

// raise makes c, which a caller of Port gave, at least as fresh as floor without comparing two tokens: Fresh stays
// fresh, and an AtLeast under a floor that is not zero becomes Fresh, which is at least as fresh as both.
func (c Consistency) raise(floor Token) Consistency {
	if floor.IsZero() || c.kind != consistencyAtLeast {
		return c
	}
	return Fresh()
}

// ErrNoConsistency is the error of an evaluation with the zero Consistency (ST-SUP-RP06-LC03).
var ErrNoConsistency = errors.New("authz: the evaluation has no consistency; give Fresh or AtLeast(token)")

// EvaluationRequest is one access evaluation, in the shape of OpenID AuthZEN 1.0: may the subject do the action
// on the resource, in the context, at the consistency.
type EvaluationRequest struct {
	Subject     Subject
	Action      Action
	Resource    Resource
	Context     Context
	Consistency Consistency
}

// EvaluationResult is the decision of one evaluation: whether it is allowed, the token the provider checked at,
// and the reason the provider gives. A deny is a result with Allowed false and is never an error
// (ST-SUP-RP06-LC01).
type EvaluationResult struct {
	Allowed bool
	Token   Token
	Reason  string
}

// Semantic is the evaluations semantic of a batch, as OpenID AuthZEN 1.0 names it.
type Semantic string

const (
	// ExecuteAll evaluates every request of the batch. It is the semantic of a batch that gives none.
	ExecuteAll Semantic = "execute_all"
	// DenyOnFirstDeny stops the batch after its first deny.
	DenyOnFirstDeny Semantic = "deny_on_first_deny"
	// PermitOnFirstPermit stops the batch after its first allow.
	PermitOnFirstPermit Semantic = "permit_on_first_permit"
)

// BatchRequest is a batch evaluation of several requests, with its semantic. An empty Semantic is ExecuteAll.
type BatchRequest struct {
	Evaluations []EvaluationRequest
	Semantic    Semantic
}

// PageToken is the opaque token of the next page of a resource search. It has no exported field: only the provider
// whose name it holds can read its cursor and the consistency of the search's first page, which every later page
// sends again unchanged (ST-SUP-RP06-LC05). The zero value is the first page.
type PageToken struct {
	provider    string
	cursor      string
	consistency Consistency
}

// NewPageToken wraps the cursor of the provider named provider with the consistency of the first page. A provider
// calls it.
func NewPageToken(provider, cursor string, first Consistency) PageToken {
	if cursor == "" {
		return PageToken{}
	}
	return PageToken{provider: provider, cursor: cursor, consistency: first}
}

// CursorFor returns the cursor and the first page's consistency of the page token for the provider named provider,
// and false when the token is the first page or belongs to another provider.
func (p PageToken) CursorFor(provider string) (string, Consistency, bool) {
	if p.cursor == "" || p.provider != provider {
		return "", Consistency{}, false
	}
	return p.cursor, p.consistency, true
}

// IsZero reports whether p is the first page, or the end of the search when a result gives it.
func (p PageToken) IsZero() bool { return p.cursor == "" }

// Page is the page of a resource search: the token of the page and the most resources to return.
type Page struct {
	Token PageToken
	Limit int
}

// SearchRequest is a resource search, in the shape of OpenID AuthZEN 1.0: the resources of one type that the
// subject may do the action on, in the context, at the consistency, one page at a time.
type SearchRequest struct {
	Subject      Subject
	Action       Action
	ResourceType string
	Context      Context
	Consistency  Consistency
	Page         Page
}

// SearchResult is one page of a resource search: the allowed resources, the token of the next page, which is zero
// at the end, and the token the provider searched at.
type SearchResult struct {
	Resources []Resource
	Next      PageToken
	Token     Token
}

// Decision is the decision port, in the shape of OpenID AuthZEN 1.0 (ST-SUP-RP06-LC01). A provider implements it;
// generated code reaches it only through Port and Handle, which check the request first.
type Decision interface {
	// Evaluate decides one request. A deny is a result with Allowed false and a nil error.
	Evaluate(ctx context.Context, request EvaluationRequest) (EvaluationResult, error)
	// EvaluateBatch decides the requests of a batch at one consistency and returns one result per request evaluated.
	EvaluateBatch(ctx context.Context, request BatchRequest) ([]EvaluationResult, error)
	// Search returns one page of the resources that the subject may do the action on. A provider that does not
	// search returns ErrNoSearch.
	Search(ctx context.Context, request SearchRequest) (SearchResult, error)
	// Searches reports whether the provider offers Search (ST-SUP-RP06-LC11).
	Searches() bool
}

// ErrNoSearch is the error of Search on a provider that does not search.
var ErrNoSearch = errors.New("authz: the provider does not search")

// Tuple is one relationship: the subject, or the subject's relation, holds the relation on the resource.
type Tuple struct {
	Resource        Resource
	Relation        string
	Subject         Subject
	SubjectRelation string
}

// Name names the tuple by its resource type, resource ID and relation only, as type:id#relation: an error, a log
// and telemetry name a tuple so, never by its subject (ST-SUP-RP06-LC20).
func (t Tuple) Name() string { return t.Resource.String() + "#" + t.Relation }

func (t Tuple) valid() error {
	if err := t.Resource.valid(); err != nil {
		return err
	}
	if err := validType("relation", t.Relation); err != nil {
		return err
	}
	if err := t.Subject.valid(); err != nil {
		return err
	}
	if t.SubjectRelation != "" {
		return validType("subject relation", t.SubjectRelation)
	}
	return nil
}

// Filter selects the relationships that Delete removes and that a precondition matches. ResourceType is required;
// each other empty field matches any value.
type Filter struct {
	ResourceType    string
	ResourceID      string
	Relation        string
	SubjectType     string
	SubjectID       string
	SubjectRelation string
}

// Exact reports whether the filter names one relationship: every field is set but SubjectRelation, which may be
// empty for a direct subject.
func (f Filter) Exact() bool {
	return f.ResourceType != "" && f.ResourceID != "" && f.Relation != "" && f.SubjectType != "" && f.SubjectID != ""
}

// FilterOf returns the exact filter of one tuple.
func FilterOf(t Tuple) Filter {
	return Filter{ResourceType: t.Resource.Type, ResourceID: t.Resource.ID, Relation: t.Relation,
		SubjectType: t.Subject.Type, SubjectID: t.Subject.ID, SubjectRelation: t.SubjectRelation}
}

// Precondition is a condition of a write: the filter must match at least one relationship, or none when MustExist
// is false. A write whose precondition fails changes nothing.
type Precondition struct {
	Filter    Filter
	MustExist bool
}

// Relationships is the relationships port (ST-SUP-RP06-LC02). Touch and Delete are idempotent on retry and return
// the token the provider wrote at. Head reads the provider's head revision with one fully consistent call, for the
// revocation floor (ST-SUP-RP06-LC13). Only the relay destination holds it: a handler, a slot fill and the handle
// never get one.
type Relationships interface {
	Touch(ctx context.Context, tuples []Tuple, preconditions []Precondition) (Token, error)
	Delete(ctx context.Context, filters []Filter, preconditions []Precondition) (Token, error)
	Head(ctx context.Context) (Token, error)
}

// refusedError marks an error of a provider that refused the request itself, such as a malformed tuple or a failed
// precondition: a retry of the same request fails again.
type refusedError struct {
	err error
}

func (e *refusedError) Error() string { return e.err.Error() }
func (e *refusedError) Unwrap() error { return e.err }

// Refused marks err as a refusal of the provider, which a retry of the same request does not change. A provider
// calls it; the relay parks the entry (ST-SUP-RP06-LC06).
func Refused(err error) error {
	if err == nil {
		return nil
	}
	return &refusedError{err: err}
}

// IsRefused reports whether err is marked by Refused.
func IsRefused(err error) bool {
	var refused *refusedError
	return errors.As(err, &refused)
}

// fixedError is an error of the package with a fixed text. It wraps only a context error of its cause, so
// errors.Is(err, context.Canceled) and errors.Is(err, context.DeadlineExceeded) hold, and no text of the cause, such
// as a provider message, a subject or a caveat value, reaches it (ST-SUP-RP06-LC20).
type fixedError struct {
	text  string
	cause error
}

func (e *fixedError) Error() string { return e.text }
func (e *fixedError) Unwrap() error { return e.cause }

// fixed returns an error with the fixed text that keeps the context error of err, if it has one.
func fixed(text string, err error) error {
	return &fixedError{text: text, cause: contextCause(err)}
}

// contextCause gives context.Canceled or context.DeadlineExceeded when err is one, or carries the gRPC status code of
// one, and nil otherwise.
func contextCause(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	}
	return nil
}

// Floor gives the service's revocation floor (ST-SUP-RP06-LC13): the token of the newest tuple write the relay
// delivered, or the zero token when there is none yet.
type Floor interface {
	Current(ctx context.Context) (Token, error)
}

// Port is the runtime's entry point to a decision provider. It refuses a request with no consistency or no subject
// before the provider is called, raises every consistency to the revocation floor, and sends ExecuteAll for a batch
// that gives no semantic. It holds no relationships port.
type Port struct {
	provider Decision
	floor    Floor
}

// NewPort returns the entry point to provider. floor gives the revocation floor; it may be nil only for a service
// with no relay, whose floor is always zero.
func NewPort(provider Decision, floor Floor) (*Port, error) {
	if provider == nil {
		return nil, errors.New("authz: no decision provider")
	}
	return &Port{provider: provider, floor: floor}, nil
}

// Searches reports whether the provider offers a resource search.
func (p *Port) Searches() bool { return p.provider.Searches() }

func (p *Port) currentFloor(ctx context.Context) (Token, error) {
	if p.floor == nil {
		return Token{}, nil
	}
	floor, err := p.floor.Current(ctx)
	if err != nil {
		return Token{}, fixed("authz: the revocation floor cannot be read", err)
	}
	return floor, nil
}

// Evaluate decides one request at least as fresh as its consistency and the revocation floor.
func (p *Port) Evaluate(ctx context.Context, request EvaluationRequest) (EvaluationResult, error) {
	floor, err := p.currentFloor(ctx)
	if err != nil {
		return EvaluationResult{}, err
	}
	return p.evaluate(ctx, request, floor)
}

func (p *Port) evaluate(ctx context.Context, request EvaluationRequest, floor Token) (EvaluationResult, error) {
	if err := checkRequest(request.Subject, request.Action, request.Consistency); err != nil {
		return EvaluationResult{}, err
	}
	if err := request.Resource.valid(); err != nil {
		return EvaluationResult{}, err
	}
	request.Consistency = request.Consistency.raise(floor)
	return p.provider.Evaluate(ctx, request)
}

// EvaluateBatch decides a batch at the consistency of its first request, made at least as fresh as the revocation
// floor, which it gives every request of the batch. A batch that gives no semantic is sent as ExecuteAll.
func (p *Port) EvaluateBatch(ctx context.Context, request BatchRequest) ([]EvaluationResult, error) {
	floor, err := p.currentFloor(ctx)
	if err != nil {
		return nil, err
	}
	return p.evaluateBatch(ctx, request, floor)
}

func (p *Port) evaluateBatch(ctx context.Context, request BatchRequest, floor Token) ([]EvaluationResult, error) {
	switch request.Semantic {
	case "":
		request.Semantic = ExecuteAll
	case ExecuteAll, DenyOnFirstDeny, PermitOnFirstPermit:
	default:
		return nil, errors.New("authz: the batch semantic is not one of execute_all, deny_on_first_deny and permit_on_first_permit")
	}
	if len(request.Evaluations) == 0 {
		return nil, nil
	}
	// A batch runs at one consistency, that of its first request, raised to the floor; no two tokens are compared.
	consistency := request.Evaluations[0].Consistency.raise(floor)
	evaluations := make([]EvaluationRequest, len(request.Evaluations))
	for i, evaluation := range request.Evaluations {
		if err := checkRequest(evaluation.Subject, evaluation.Action, evaluation.Consistency); err != nil {
			return nil, err
		}
		if err := evaluation.Resource.valid(); err != nil {
			return nil, err
		}
		evaluation.Consistency = consistency
		evaluations[i] = evaluation
	}
	request.Evaluations = evaluations
	return p.provider.EvaluateBatch(ctx, request)
}

// Search returns one page of a resource search at least as fresh as its consistency and the revocation floor, or
// ErrNoSearch when the provider does not search.
func (p *Port) Search(ctx context.Context, request SearchRequest) (SearchResult, error) {
	floor, err := p.currentFloor(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	return p.search(ctx, request, floor)
}

func (p *Port) search(ctx context.Context, request SearchRequest, floor Token) (SearchResult, error) {
	if !p.provider.Searches() {
		return SearchResult{}, ErrNoSearch
	}
	if err := checkRequest(request.Subject, request.Action, request.Consistency); err != nil {
		return SearchResult{}, err
	}
	if err := validType("resource", request.ResourceType); err != nil {
		return SearchResult{}, err
	}
	if request.Page.Limit < 0 {
		return SearchResult{}, errors.New("authz: the page limit is negative")
	}
	request.Consistency = request.Consistency.raise(floor)
	return p.provider.Search(ctx, request)
}

func checkRequest(subject Subject, action Action, consistency Consistency) error {
	if consistency.IsZero() {
		return ErrNoConsistency
	}
	if err := subject.valid(); err != nil {
		return err
	}
	return validType("action", action.Name)
}

// validType checks a resource type, a subject type, a relation or a permission name as SpiceDB writes them.
func validType(kind, name string) error {
	if name == "" {
		return fmt.Errorf("authz: the %s is empty", kind)
	}
	if len(name) > 128 {
		return fmt.Errorf("authz: the %s is longer than 128 bytes", kind)
	}
	for i, r := range name {
		ok := r == '_' || r == '/' || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("authz: the %s holds a character other than a-z, 0-9, _ and /", kind)
		}
	}
	return nil
}

// validID checks the ID of a resource or a subject. The wildcard * is refused: no subject or resource stands for
// all (ST-SUP-RP06-LC07).
func validID(kind, id string) error {
	if id == "" {
		return fmt.Errorf("authz: the %s ID is empty", kind)
	}
	if len(id) > 1024 {
		return fmt.Errorf("authz: the %s ID is longer than 1024 bytes", kind)
	}
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			strings.ContainsRune("/_|-=+", r)
		if !ok {
			return fmt.Errorf("authz: the %s ID holds a character that a provider ID cannot hold", kind)
		}
	}
	return nil
}

// cleanText reports whether s is valid UTF-8 with no control character, so it can go to a text column.
func cleanText(s string) bool {
	for _, r := range s {
		if r == 0xFFFD || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

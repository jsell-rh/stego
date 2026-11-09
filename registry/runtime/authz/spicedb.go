package authz

import (
	"context"
	"errors"
	"io"
	"sort"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// spiceDBName is the provider name in the page tokens of the SpiceDB provider.
const spiceDBName = "spicedb"

// SpiceDB is the SpiceDB provider of the decision and relationships ports (ST-SUP-RP06-LC05). It speaks gRPC over
// the connection that the service's own code opens, with its TLS and its preshared key, and opens no other
// transport. It maps Fresh to fully_consistent and AtLeast to at_least_as_fresh, writes with TOUCH and DELETE only,
// and wraps the after_result_cursor of a lookup, with the first page's consistency, in the opaque page token. It
// has no exact-snapshot path, and it uses the ZedTokens of every datastore as they are, without parsing them.
type SpiceDB struct {
	permissions v1.PermissionsServiceClient
	schema      v1.SchemaServiceClient
}

// NewSpiceDB returns the SpiceDB provider on conn.
func NewSpiceDB(conn grpc.ClientConnInterface) (*SpiceDB, error) {
	if conn == nil {
		return nil, errors.New("authz: no SpiceDB connection")
	}
	return &SpiceDB{permissions: v1.NewPermissionsServiceClient(conn), schema: v1.NewSchemaServiceClient(conn)}, nil
}

// Searches reports that SpiceDB offers a resource search, through LookupResources.
func (s *SpiceDB) Searches() bool { return true }

// Evaluate decides one request with CheckPermission. A conditional permission is a ContextError, not a deny
// (ST-SUP-RP06-LC08).
func (s *SpiceDB) Evaluate(ctx context.Context, request EvaluationRequest) (EvaluationResult, error) {
	consistency, err := spiceConsistency(request.Consistency)
	if err != nil {
		return EvaluationResult{}, err
	}
	caveat, err := spiceContext(request.Context)
	if err != nil {
		return EvaluationResult{}, err
	}
	response, err := s.permissions.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Consistency: consistency,
		Resource:    spiceObject(request.Resource.Type, request.Resource.ID),
		Permission:  request.Action.Name,
		Subject:     spiceSubject(request.Subject, ""),
		Context:     caveat,
	})
	if err != nil {
		return EvaluationResult{}, spiceError("check the permission", err)
	}
	token, err := tokenOf(response.GetCheckedAt())
	if err != nil {
		return EvaluationResult{}, err
	}
	return resultOf(response.GetPermissionship(), response.GetPartialCaveatInfo(), token)
}

// EvaluateBatch decides a batch with one CheckBulkPermissions call, at the consistency of the first request, which
// Port and Handle give every request of a batch. ExecuteAll returns a result for every request;
// DenyOnFirstDeny and PermitOnFirstPermit return the results up to and with the first deny or allow.
func (s *SpiceDB) EvaluateBatch(ctx context.Context, request BatchRequest) ([]EvaluationResult, error) {
	if len(request.Evaluations) == 0 {
		return nil, nil
	}
	consistency, err := spiceConsistency(request.Evaluations[0].Consistency)
	if err != nil {
		return nil, err
	}
	items := make([]*v1.CheckBulkPermissionsRequestItem, len(request.Evaluations))
	for i, evaluation := range request.Evaluations {
		caveat, err := spiceContext(evaluation.Context)
		if err != nil {
			return nil, err
		}
		items[i] = &v1.CheckBulkPermissionsRequestItem{
			Resource:   spiceObject(evaluation.Resource.Type, evaluation.Resource.ID),
			Permission: evaluation.Action.Name,
			Subject:    spiceSubject(evaluation.Subject, ""),
			Context:    caveat,
		}
	}
	response, err := s.permissions.CheckBulkPermissions(ctx, &v1.CheckBulkPermissionsRequest{
		Consistency: consistency,
		Items:       items,
	})
	if err != nil {
		return nil, spiceError("check the batch", err)
	}
	if len(response.GetPairs()) != len(items) {
		return nil, errors.New("authz: SpiceDB did not answer every check of the batch")
	}
	token, err := tokenOf(response.GetCheckedAt())
	if err != nil {
		return nil, err
	}
	results := make([]EvaluationResult, 0, len(items))
	for _, pair := range response.GetPairs() {
		if pairError := pair.GetError(); pairError != nil {
			return nil, spiceError("check the batch", status.ErrorProto(pairError))
		}
		item := pair.GetItem()
		result, err := resultOf(item.GetPermissionship(), item.GetPartialCaveatInfo(), token)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
		if (request.Semantic == DenyOnFirstDeny && !result.Allowed) || (request.Semantic == PermitOnFirstPermit && result.Allowed) {
			break
		}
	}
	return results, nil
}

// Search returns one page of a resource search with LookupResources. The page token carries the
// after_result_cursor of the last resource of the page and the consistency of the first page, which each later page
// sends again unchanged, because SpiceDB binds the consistency into the cursor. The results of a later page are
// then filtered with one CheckBulkPermissions call at the request's consistency, which the handle sets at the
// floor of that page, so a later page is in force at the current floor (ST-SUP-RP06-LC05).
func (s *SpiceDB) Search(ctx context.Context, request SearchRequest) (SearchResult, error) {
	lookupConsistency := request.Consistency
	cursor, first, later := request.Page.Token.CursorFor(spiceDBName)
	if later {
		lookupConsistency = first
	} else if !request.Page.Token.IsZero() {
		return SearchResult{}, errors.New("authz: the page token is not a SpiceDB page token")
	}
	consistency, err := spiceConsistency(lookupConsistency)
	if err != nil {
		return SearchResult{}, err
	}
	caveat, err := spiceContext(request.Context)
	if err != nil {
		return SearchResult{}, err
	}
	lookup := &v1.LookupResourcesRequest{
		Consistency:        consistency,
		ResourceObjectType: request.ResourceType,
		Permission:         request.Action.Name,
		Subject:            spiceSubject(request.Subject, ""),
		Context:            caveat,
	}
	if request.Page.Limit > 0 {
		lookup.OptionalLimit = uint32(request.Page.Limit)
	}
	if later {
		lookup.OptionalCursor = &v1.Cursor{Token: cursor}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := s.permissions.LookupResources(ctx, lookup)
	if err != nil {
		return SearchResult{}, spiceError("search the resources", err)
	}
	var result SearchResult
	var last string
	found := 0
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return SearchResult{}, spiceError("search the resources", err)
		}
		found++
		last = response.GetAfterResultCursor().GetToken()
		if response.GetPermissionship() == v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_CONDITIONAL_PERMISSION {
			return SearchResult{}, contextError(response.GetPartialCaveatInfo())
		}
		if response.GetPermissionship() != v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_HAS_PERMISSION {
			continue
		}
		result.Resources = append(result.Resources, Resource{Type: request.ResourceType, ID: response.GetResourceObjectId()})
		if response.GetLookedUpAt() != nil {
			if result.Token, err = tokenOf(response.GetLookedUpAt()); err != nil {
				return SearchResult{}, err
			}
		}
	}
	// A full page may have more after it; a short page is the end.
	if request.Page.Limit > 0 && found == request.Page.Limit && last != "" {
		result.Next = NewPageToken(spiceDBName, last, lookupConsistency)
	}
	if later && len(result.Resources) > 0 {
		evaluations := make([]EvaluationRequest, len(result.Resources))
		for i, resource := range result.Resources {
			evaluations[i] = EvaluationRequest{Subject: request.Subject, Action: request.Action, Resource: resource,
				Context: request.Context, Consistency: request.Consistency}
		}
		checked, err := s.EvaluateBatch(ctx, BatchRequest{Evaluations: evaluations, Semantic: ExecuteAll})
		if err != nil {
			return SearchResult{}, err
		}
		var allowed []Resource
		for i, check := range checked {
			if check.Allowed {
				allowed = append(allowed, result.Resources[i])
			}
			result.Token = check.Token
		}
		result.Resources = allowed
	}
	return result, nil
}

// Touch writes the tuples with OPERATION_TOUCH in one WriteRelationships request, which is idempotent on retry.
func (s *SpiceDB) Touch(ctx context.Context, tuples []Tuple, preconditions []Precondition) (Token, error) {
	if len(tuples) == 0 {
		return Token{}, errors.New("authz: Touch needs at least one tuple")
	}
	updates := make([]*v1.RelationshipUpdate, len(tuples))
	for i, tuple := range tuples {
		if err := tuple.valid(); err != nil {
			return Token{}, Refused(err)
		}
		updates[i] = &v1.RelationshipUpdate{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: spiceObject(tuple.Resource.Type, tuple.Resource.ID),
				Relation: tuple.Relation,
				Subject:  spiceSubject(tuple.Subject, tuple.SubjectRelation),
			},
		}
	}
	return s.write(ctx, updates, preconditions)
}

// Delete removes the relationships of the filters, which is idempotent on retry. Exact filters go in one
// WriteRelationships request with OPERATION_DELETE; one filter that is not exact goes to DeleteRelationships, and
// more than one is refused, because two calls would not be atomic.
func (s *SpiceDB) Delete(ctx context.Context, filters []Filter, preconditions []Precondition) (Token, error) {
	if len(filters) == 0 {
		return Token{}, errors.New("authz: Delete needs at least one filter")
	}
	exact := true
	for _, filter := range filters {
		if filter.ResourceType == "" {
			return Token{}, Refused(errors.New("authz: a filter needs a resource type"))
		}
		exact = exact && filter.Exact()
	}
	if exact {
		updates := make([]*v1.RelationshipUpdate, len(filters))
		for i, filter := range filters {
			updates[i] = &v1.RelationshipUpdate{
				Operation: v1.RelationshipUpdate_OPERATION_DELETE,
				Relationship: &v1.Relationship{
					Resource: spiceObject(filter.ResourceType, filter.ResourceID),
					Relation: filter.Relation,
					Subject: spiceSubject(Subject{Type: filter.SubjectType, ID: filter.SubjectID},
						filter.SubjectRelation),
				},
			}
		}
		return s.write(ctx, updates, preconditions)
	}
	if len(filters) > 1 {
		return Token{}, Refused(errors.New("authz: Delete takes more than one filter only when each names one relationship"))
	}
	response, err := s.permissions.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter:    spiceFilter(filters[0]),
		OptionalPreconditions: spicePreconditions(preconditions),
	})
	if err != nil {
		return Token{}, spiceError("delete the relationships", err)
	}
	return tokenOf(response.GetDeletedAt())
}

// Head reads the head revision of SpiceDB with ReadSchema, which SpiceDB always answers at its head revision, so
// the token is at least as fresh as every write that committed before the call (ST-SUP-RP06-LC13).
func (s *SpiceDB) Head(ctx context.Context) (Token, error) {
	response, err := s.schema.ReadSchema(ctx, &v1.ReadSchemaRequest{})
	if err != nil {
		return Token{}, spiceError("read the head revision", err)
	}
	return tokenOf(response.GetReadAt())
}

func (s *SpiceDB) write(ctx context.Context, updates []*v1.RelationshipUpdate, preconditions []Precondition) (Token, error) {
	response, err := s.permissions.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates:               updates,
		OptionalPreconditions: spicePreconditions(preconditions),
	})
	if err != nil {
		return Token{}, spiceError("write the relationships", err)
	}
	return tokenOf(response.GetWrittenAt())
}

func spiceObject(objectType, id string) *v1.ObjectReference {
	return &v1.ObjectReference{ObjectType: objectType, ObjectId: id}
}

func spiceSubject(subject Subject, relation string) *v1.SubjectReference {
	return &v1.SubjectReference{Object: spiceObject(subject.Type, subject.ID), OptionalRelation: relation}
}

func spiceFilter(filter Filter) *v1.RelationshipFilter {
	out := &v1.RelationshipFilter{
		ResourceType:       filter.ResourceType,
		OptionalResourceId: filter.ResourceID,
		OptionalRelation:   filter.Relation,
	}
	if filter.SubjectType != "" {
		out.OptionalSubjectFilter = &v1.SubjectFilter{SubjectType: filter.SubjectType, OptionalSubjectId: filter.SubjectID}
		if filter.SubjectRelation != "" {
			out.OptionalSubjectFilter.OptionalRelation = &v1.SubjectFilter_RelationFilter{Relation: filter.SubjectRelation}
		}
	}
	return out
}

func spicePreconditions(preconditions []Precondition) []*v1.Precondition {
	if len(preconditions) == 0 {
		return nil
	}
	out := make([]*v1.Precondition, len(preconditions))
	for i, precondition := range preconditions {
		operation := v1.Precondition_OPERATION_MUST_NOT_MATCH
		if precondition.MustExist {
			operation = v1.Precondition_OPERATION_MUST_MATCH
		}
		out[i] = &v1.Precondition{Operation: operation, Filter: spiceFilter(precondition.Filter)}
	}
	return out
}

// spiceConsistency maps Fresh to fully_consistent and AtLeast to at_least_as_fresh. Every other value is refused:
// SpiceDB's minimize_latency and at_exact_snapshot have no Consistency value (ST-SUP-RP06-LC03).
func spiceConsistency(consistency Consistency) (*v1.Consistency, error) {
	if consistency.IsFresh() {
		return &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}}, nil
	}
	if token, ok := consistency.Token(); ok {
		return &v1.Consistency{Requirement: &v1.Consistency_AtLeastAsFresh{
			AtLeastAsFresh: &v1.ZedToken{Token: token.Value()},
		}}, nil
	}
	return nil, ErrNoConsistency
}

func spiceContext(context Context) (*structpb.Struct, error) {
	if len(context) == 0 {
		return nil, nil
	}
	caveat, err := structpb.NewStruct(context)
	if err != nil {
		return nil, errors.New("authz: the caveat context holds a value that is not JSON")
	}
	return caveat, nil
}

func resultOf(permissionship v1.CheckPermissionResponse_Permissionship, caveat *v1.PartialCaveatInfo, token Token) (EvaluationResult, error) {
	switch permissionship {
	case v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION:
		return EvaluationResult{Allowed: true, Token: token, Reason: "has_permission"}, nil
	case v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION:
		return EvaluationResult{Allowed: false, Token: token, Reason: "no_permission"}, nil
	case v1.CheckPermissionResponse_PERMISSIONSHIP_CONDITIONAL_PERMISSION:
		return EvaluationResult{}, contextError(caveat)
	default:
		return EvaluationResult{}, errors.New("authz: SpiceDB answered an unknown permissionship")
	}
}

func contextError(caveat *v1.PartialCaveatInfo) error {
	missing := append([]string(nil), caveat.GetMissingRequiredContext()...)
	sort.Strings(missing)
	return &ContextError{Missing: missing}
}

// ErrProvider matches every error of a provider call: errors.Is(err, ErrProvider). IsRefused tells a refusal, which
// a retry does not change, from an error that a retry may pass; a ContextError is a third kind.
var ErrProvider = errors.New("authz: the provider call failed")

// providerError is the error of a provider call. Its text is fixed for the call and the gRPC status code, and it
// wraps no error of the provider, so no provider message, subject ID or caveat value reaches it (ST-SUP-RP06-LC20).
type providerError struct {
	what  string
	code  codes.Code
	cause error
}

func (e *providerError) Error() string {
	return "authz: SpiceDB: " + e.what + ": " + e.code.String()
}

func (e *providerError) Is(target error) bool { return target == ErrProvider }

// Unwrap gives the context error of a call that its context ended, so the kind stays readable.
func (e *providerError) Unwrap() error { return e.cause }

// spiceError maps a gRPC error of SpiceDB to a fixed text for what and its status code. InvalidArgument,
// FailedPrecondition, NotFound and OutOfRange are refusals that a retry does not change; any other error may pass on
// a retry.
func spiceError(what string, err error) error {
	code := status.Code(err)
	fixed := &providerError{what: what, code: code, cause: contextCause(err)}
	switch code {
	case codes.Canceled:
		fixed.cause = context.Canceled
	case codes.DeadlineExceeded:
		fixed.cause = context.DeadlineExceeded
	}
	switch code {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.NotFound, codes.OutOfRange:
		return Refused(fixed)
	default:
		return fixed
	}
}

// tokenOf makes the Token of a ZedToken. The ZedToken is kept byte for byte and never parsed: the revision of the
// PostgreSQL datastore is a snapshot, and only SpiceDB orders its tokens (ST-SUP-RP06-LC04).
func tokenOf(token *v1.ZedToken) (Token, error) {
	if token.GetToken() == "" {
		return Token{}, errors.New("authz: SpiceDB answered with no ZedToken")
	}
	return NewToken(token.GetToken())
}

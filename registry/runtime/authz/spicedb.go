package authz

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/structpb"
)

// spiceDBName is the provider name in the page tokens of the SpiceDB provider.
const spiceDBName = "spicedb"

// SpiceDB is the SpiceDB provider of the decision and relationships ports (ST-SUP-RP06-LC05). It speaks gRPC over
// the connection that the service's own code opens, with its TLS and its preshared key, and opens no other
// transport. It maps Fresh to fully_consistent and AtLeast to at_least_as_fresh, writes with TOUCH and DELETE only,
// and wraps the after_result_cursor of a lookup in the opaque page token. It has no exact-snapshot path.
type SpiceDB struct {
	permissions v1.PermissionsServiceClient
}

// NewSpiceDB returns the SpiceDB provider on conn.
func NewSpiceDB(conn grpc.ClientConnInterface) (*SpiceDB, error) {
	if conn == nil {
		return nil, errors.New("authz: no SpiceDB connection")
	}
	return &SpiceDB{permissions: v1.NewPermissionsServiceClient(conn)}, nil
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
// Port and Handle make the same for every request of a batch. ExecuteAll returns a result for every request;
// DenyOnFirstDeny and PermitOnFirstPermit return the results up to and with the first deny or allow.
func (s *SpiceDB) EvaluateBatch(ctx context.Context, request BatchRequest) ([]EvaluationResult, error) {
	if len(request.Evaluations) == 0 {
		return nil, nil
	}
	consistency, err := spiceConsistency(batchConsistency(request.Evaluations))
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
		return nil, fmt.Errorf("authz: SpiceDB answered %d of the %d checks of the batch", len(response.GetPairs()), len(items))
	}
	token, err := tokenOf(response.GetCheckedAt())
	if err != nil {
		return nil, err
	}
	results := make([]EvaluationResult, 0, len(items))
	for i, pair := range response.GetPairs() {
		if pairError := pair.GetError(); pairError != nil {
			return nil, spiceError(fmt.Sprintf("check %d of the batch", i), status.ErrorProto(pairError))
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
// after_result_cursor of the last resource of the page.
func (s *SpiceDB) Search(ctx context.Context, request SearchRequest) (SearchResult, error) {
	consistency, err := spiceConsistency(request.Consistency)
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
	if cursor, ok := request.Page.Token.CursorFor(spiceDBName); ok {
		lookup.OptionalCursor = &v1.Cursor{Token: cursor}
	} else if !request.Page.Token.IsZero() {
		return SearchResult{}, errors.New("authz: the page token is not a SpiceDB page token")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := s.permissions.LookupResources(ctx, lookup)
	if err != nil {
		return SearchResult{}, spiceError("search the resources", err)
	}
	var result SearchResult
	var last string
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return SearchResult{}, spiceError("search the resources", err)
		}
		if response.GetPermissionship() == v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_CONDITIONAL_PERMISSION {
			return SearchResult{}, contextError(response.GetPartialCaveatInfo())
		}
		if response.GetPermissionship() != v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_HAS_PERMISSION {
			continue
		}
		result.Resources = append(result.Resources, Resource{Type: request.ResourceType, ID: response.GetResourceObjectId()})
		last = response.GetAfterResultCursor().GetToken()
		if response.GetLookedUpAt() != nil {
			token, err := tokenOf(response.GetLookedUpAt())
			if err != nil {
				return SearchResult{}, err
			}
			result.Token = token
		}
	}
	// A full page may have more after it; a short page is the end.
	if request.Page.Limit > 0 && len(result.Resources) == request.Page.Limit {
		result.Next = NewPageToken(spiceDBName, last)
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

// batchConsistency is the freshest consistency of the requests of a batch: Fresh when one is fresh, otherwise
// AtLeast the freshest token.
func batchConsistency(evaluations []EvaluationRequest) Consistency {
	var token Token
	for _, evaluation := range evaluations {
		if evaluation.Consistency.IsFresh() {
			return Fresh()
		}
		if t, ok := evaluation.Consistency.Token(); ok {
			token = fresher(token, t)
		}
	}
	return AtLeast(token)
}

func spiceContext(context Context) (*structpb.Struct, error) {
	if len(context) == 0 {
		return nil, nil
	}
	caveat, err := structpb.NewStruct(context)
	if err != nil {
		return nil, fmt.Errorf("authz: the caveat context: %w", err)
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
		return EvaluationResult{}, fmt.Errorf("authz: SpiceDB answered the permissionship %s", permissionship)
	}
}

func contextError(caveat *v1.PartialCaveatInfo) error {
	missing := append([]string(nil), caveat.GetMissingRequiredContext()...)
	sort.Strings(missing)
	return &ContextError{Missing: missing}
}

// spiceError maps a gRPC error of SpiceDB. InvalidArgument, FailedPrecondition and NotFound are refusals that a
// retry does not change; any other error may pass on a retry.
func spiceError(what string, err error) error {
	wrapped := fmt.Errorf("authz: SpiceDB: %s: %w", what, err)
	switch status.Code(err) {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.NotFound, codes.OutOfRange:
		return Refused(wrapped)
	default:
		return wrapped
	}
}

// tokenOf makes the Token of a ZedToken. Its revision comes from the token's own encoding: the base64 of a
// DecodedZedToken whose v1 message holds the datastore revision as text, or whose deprecated v1 zookie holds it as
// a number. A revision that is not a decimal number, such as one of the PostgreSQL datastore, is refused, because
// the revocation floor needs revisions it can order.
func tokenOf(token *v1.ZedToken) (Token, error) {
	if token.GetToken() == "" {
		return Token{}, errors.New("authz: SpiceDB answered with no ZedToken")
	}
	revision, err := zedRevision(token.GetToken())
	if err != nil {
		return Token{}, err
	}
	return NewToken(token.GetToken(), revision)
}

// Fields of SpiceDB's DecodedZedToken (proto/internal/impl/v1/impl.proto).
const (
	zedDeprecatedZookieField = 2
	zedV1Field               = 3
	zedRevisionField         = 1
)

func zedRevision(text string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return "", fmt.Errorf("authz: the ZedToken is not base64: %w", err)
	}
	for len(raw) > 0 {
		number, kind, n := protowire.ConsumeTag(raw)
		if n < 0 {
			return "", errors.New("authz: the ZedToken does not decode")
		}
		raw = raw[n:]
		if kind != protowire.BytesType || (number != zedV1Field && number != zedDeprecatedZookieField) {
			n = protowire.ConsumeFieldValue(number, kind, raw)
			if n < 0 {
				return "", errors.New("authz: the ZedToken does not decode")
			}
			raw = raw[n:]
			continue
		}
		inner, n := protowire.ConsumeBytes(raw)
		if n < 0 {
			return "", errors.New("authz: the ZedToken does not decode")
		}
		return innerRevision(number, inner)
	}
	return "", errors.New("authz: the ZedToken holds no revision")
}

func innerRevision(version protowire.Number, raw []byte) (string, error) {
	for len(raw) > 0 {
		number, kind, n := protowire.ConsumeTag(raw)
		if n < 0 {
			return "", errors.New("authz: the ZedToken does not decode")
		}
		raw = raw[n:]
		switch {
		case number == zedRevisionField && version == zedV1Field && kind == protowire.BytesType:
			value, n := protowire.ConsumeBytes(raw)
			if n < 0 {
				return "", errors.New("authz: the ZedToken does not decode")
			}
			revision := string(value)
			if _, err := parseRevision(revision); err != nil {
				return "", fmt.Errorf("authz: the SpiceDB datastore gives revisions that cannot be ordered: %w", err)
			}
			return revision, nil
		case number == zedRevisionField && version == zedDeprecatedZookieField && kind == protowire.VarintType:
			value, n := protowire.ConsumeVarint(raw)
			if n < 0 {
				return "", errors.New("authz: the ZedToken does not decode")
			}
			return strconv.FormatUint(value, 10), nil
		default:
			n = protowire.ConsumeFieldValue(number, kind, raw)
			if n < 0 {
				return "", errors.New("authz: the ZedToken does not decode")
			}
			raw = raw[n:]
		}
	}
	return "", errors.New("authz: the ZedToken holds no revision")
}

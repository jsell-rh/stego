package authz

import (
	"context"
	"errors"
	"fmt"
)

// Permission is one permission that the conformance suite checks: a subject that the grant tuple gives the
// permission on the resource must be allowed, and a subject with no relation must be denied.
type Permission struct {
	// Name is the permission, such as view.
	Name string
	// Grant is a tuple that gives its subject the permission on its resource, such as document:c1#viewer@user:alice
	// for view.
	Grant Tuple
	// Stranger is a subject with no relation to the resource.
	Stranger Subject
}

// Suite is what the conformance suite runs against: the provider's two ports and the permissions of the declared
// schema, which the provider must have loaded.
type Suite struct {
	Decision      Decision
	Relationships Relationships
	Permissions   []Permission
}

// CaseResult is the result of one case of the suite.
type CaseResult struct {
	Name   string
	Passed bool
	Detail string
}

// Report is the result of a run of the suite.
type Report struct {
	// Cases holds one allow and one deny case per permission and the new-enemy case.
	Cases []CaseResult
	// Search holds the cases of the search group; it is empty when the provider does not search.
	Search []CaseResult
	// Searches reports whether the provider searches. When it does not, the search group is not run and every list
	// uses batch evaluation.
	Searches bool
	// Note says what the run left out, such as the search group of a provider that does not search.
	Note string
}

// Passed reports whether every case of the run passed.
func (r Report) Passed() bool {
	if len(r.Cases) == 0 {
		return false
	}
	for _, c := range append(append([]CaseResult(nil), r.Cases...), r.Search...) {
		if !c.Passed {
			return false
		}
	}
	return true
}

// Conform runs the conformance suite that every provider passes (ST-SUP-RP06-LC11): for each permission, one allow
// and one deny; the new-enemy case, in which a check at least as fresh as the token of a removed relation denies;
// and, as an optional group, a search for the allowed resource. A provider that does not search reports so, and
// the group is not run. Each case checks at least as fresh as the token of its own write, through Port, so a
// provider that answers from an older state fails.
func Conform(ctx context.Context, suite Suite) (Report, error) {
	if suite.Decision == nil || suite.Relationships == nil {
		return Report{}, errors.New("authz: the suite needs the decision port and the relationships port")
	}
	if len(suite.Permissions) == 0 {
		return Report{}, errors.New("authz: the suite needs at least one permission")
	}
	port, err := NewPort(suite.Decision, nil)
	if err != nil {
		return Report{}, err
	}
	report := Report{Searches: suite.Decision.Searches()}
	for _, permission := range suite.Permissions {
		report.Cases = append(report.Cases, allowAndDeny(ctx, port, suite.Relationships, permission)...)
	}
	report.Cases = append(report.Cases, newEnemy(ctx, port, suite.Relationships, suite.Permissions[0]))
	if !report.Searches {
		report.Note = "the provider does not search: the search group is not run, and every list uses batch evaluation"
		return report, nil
	}
	report.Search = append(report.Search, searchCase(ctx, port, suite.Relationships, suite.Permissions[0]))
	return report, nil
}

func allowAndDeny(ctx context.Context, port *Port, relationships Relationships, permission Permission) []CaseResult {
	allow := CaseResult{Name: "allow " + permission.Name}
	deny := CaseResult{Name: "deny " + permission.Name}
	token, err := relationships.Touch(ctx, []Tuple{permission.Grant}, nil)
	if err != nil {
		allow.Detail = fmt.Sprintf("touch %s: %v", permission.Grant, err)
		deny.Detail = allow.Detail
		return []CaseResult{allow, deny}
	}
	allow.Passed, allow.Detail = expect(ctx, port, permission.Grant.Subject, permission, token, true)
	deny.Passed, deny.Detail = expect(ctx, port, permission.Stranger, permission, token, false)
	return []CaseResult{allow, deny}
}

func newEnemy(ctx context.Context, port *Port, relationships Relationships, permission Permission) CaseResult {
	result := CaseResult{Name: "new enemy " + permission.Name}
	if _, err := relationships.Touch(ctx, []Tuple{permission.Grant}, nil); err != nil {
		result.Detail = fmt.Sprintf("touch %s: %v", permission.Grant, err)
		return result
	}
	token, err := relationships.Delete(ctx, []Filter{FilterOf(permission.Grant)}, nil)
	if err != nil {
		result.Detail = fmt.Sprintf("delete %s: %v", permission.Grant, err)
		return result
	}
	result.Passed, result.Detail = expect(ctx, port, permission.Grant.Subject, permission, token, false)
	return result
}

func searchCase(ctx context.Context, port *Port, relationships Relationships, permission Permission) CaseResult {
	result := CaseResult{Name: "search " + permission.Name}
	token, err := relationships.Touch(ctx, []Tuple{permission.Grant}, nil)
	if err != nil {
		result.Detail = fmt.Sprintf("touch %s: %v", permission.Grant, err)
		return result
	}
	page := Page{}
	for range 1000 {
		found, err := port.Search(ctx, SearchRequest{Subject: permission.Grant.Subject,
			Action: Action{Name: permission.Name}, ResourceType: permission.Grant.Resource.Type,
			Consistency: AtLeast(token), Page: page})
		if err != nil {
			result.Detail = fmt.Sprintf("search: %v", err)
			return result
		}
		for _, resource := range found.Resources {
			if resource == permission.Grant.Resource {
				result.Passed = true
				return result
			}
		}
		if found.Next.IsZero() {
			break
		}
		page.Token = found.Next
	}
	result.Detail = fmt.Sprintf("the search did not find %s", permission.Grant.Resource)
	return result
}

func expect(ctx context.Context, port *Port, subject Subject, permission Permission, token Token, allowed bool) (bool, string) {
	result, err := port.Evaluate(ctx, EvaluationRequest{Subject: subject, Action: Action{Name: permission.Name},
		Resource: permission.Grant.Resource, Consistency: AtLeast(token)})
	if err != nil {
		return false, fmt.Sprintf("evaluate %s on %s for %s: %v", permission.Name, permission.Grant.Resource, subject, err)
	}
	if result.Allowed != allowed {
		return false, fmt.Sprintf("%s on %s for %s: allowed %t, want %t", permission.Name, permission.Grant.Resource,
			subject, result.Allowed, allowed)
	}
	return true, ""
}

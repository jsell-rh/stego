package kubernetesservice

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
)

type allocationPodRule struct{ Expression, Message string }

// Application rules can only add denials to the generated Pod policy. They
// cannot change its scope, failure mode, variables, or common checks.
func allocationPodRules(values object, p *allocationProfile) error {
	raw, exists := values["pod_validations"]
	if !exists {
		return nil
	}
	entries, ok := raw.([]any)
	if !ok || len(entries) == 0 || len(entries) > 16 || (p.PodRuntimeClass == "" && p.PodServiceAccount == "") {
		return fmt.Errorf("pod_validations requires 1..16 rules and a Pod runtime or account restriction")
	}
	// Use only standard CEL. Kubernetes performs the final check against the
	// installed Pod schema. Dynamic fields here do not prove that schema check.
	env, err := cel.NewEnv(
		cel.Variable("object", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("oldObject", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("namespaceObject", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("request", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("variables.containers", cel.ListType(cel.DynType)),
		cel.ParserExpressionSizeLimit(4096), cel.ParserRecursionLimit(64),
		cel.ParserErrorRecoveryLimit(8), cel.ExpressionNodeLimit(2048),
	)
	if err != nil {
		return fmt.Errorf("create Pod rule environment: %w", err)
	}
	seen := map[string]bool{}
	for i, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok || len(entry) != 2 {
			return fmt.Errorf("Pod rule %d requires expression and message", i+1)
		}
		expression, eok := entry["expression"].(string)
		message, mok := entry["message"].(string)
		if !eok || !mok || len(expression) == 0 || len(expression) > 4096 || len(message) == 0 || len(message) > 256 || !utf8.ValidString(expression) || !utf8.ValidString(message) || strings.TrimSpace(expression) != expression || strings.TrimSpace(message) != message || strings.ContainsAny(expression, "\x00") || strings.Contains(expression, "{{") || strings.Contains(expression, "}}") || strings.Contains(message, "{{") || strings.Contains(message, "}}") || strings.ContainsFunc(message, unicode.IsControl) || seen[expression] {
			return fmt.Errorf("Pod rule %d has an invalid expression or message", i+1)
		}
		ast, issues := env.Compile(expression)
		if issues != nil && issues.Err() != nil {
			return fmt.Errorf("Pod rule %d is not valid CEL: %w", i+1, issues.Err())
		}
		if !ast.OutputType().IsExactType(cel.BoolType) {
			return fmt.Errorf("Pod rule %d must return a boolean", i+1)
		}
		seen[expression] = true
		p.PodValidations = append(p.PodValidations, allocationPodRule{Expression: expression, Message: message})
	}
	return nil
}

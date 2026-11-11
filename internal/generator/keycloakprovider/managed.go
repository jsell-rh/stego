package keycloakprovider

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

// managedAttribute is one declared ownership attribute. Value is either a
// literal or a "{name}" placeholder that refers to a declared id; ValueExpr is
// the rendered Go expression for it.
type managedAttribute struct {
	Key       string
	Value     string
	ValueExpr string
}

// KeyExpr renders the key as a quoted Go string literal.
func (a managedAttribute) KeyExpr() string { return strconv.Quote(a.Key) }

// OwnerKeyExpr renders the key with the reserved ownership prefix as a quoted Go string literal.
func (a managedAttribute) OwnerKeyExpr() string { return strconv.Quote(ownershipPrefix + a.Key) }

// managedKind is one declared managed-client kind. The generator derives the
// exported identity and binding helpers from it.
type managedKind struct {
	Kind           string
	Name           string
	IDs            []string
	ClientID       string
	Attributes     []managedAttribute
	Legacy         bool
	LegacyClientID bool
}

var managedKindSegment = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
var managedIDName = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
var managedPlaceholder = regexp.MustCompile(`^\{([a-z][a-zA-Z0-9]*)\}$`)

// managedAttributeKey is the form of a declared ownership attribute key: lower-case letters, digits, dots, hyphens
// and underscores, which the reserved stego.owner. prefix keeps within the Keycloak attribute name limit.
var managedAttributeKey = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,110}$`)

// clientIDPlaceholder finds each {name} placeholder of a client-name template.
var clientIDPlaceholder = regexp.MustCompile(`\{([^{}]*)\}`)

const ownershipPrefix = "stego.owner."

// parseManagedKinds reads the managed_clients component settings. It returns
// nil when the application declares none.
func parseManagedKinds(ctx gen.Context) ([]managedKind, error) {
	for key := range ctx.ComponentConfig {
		if key != "managed_clients" {
			return nil, fmt.Errorf("keycloak-provider accepts only managed_clients settings, not %q", key)
		}
	}
	value, present := ctx.ComponentConfig["managed_clients"]
	if !present {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("managed_clients must be a list of managed-client kinds")
	}
	var kinds []managedKind
	seenKind := map[string]bool{}
	for index, entry := range list {
		kind, err := parseManagedKind(fmt.Sprintf("managed_clients[%d]", index), entry)
		if err != nil {
			return nil, err
		}
		if seenKind[kind.Kind] {
			return nil, fmt.Errorf("managed_clients[%d]: kind %q is declared twice", index, kind.Kind)
		}
		seenKind[kind.Kind] = true
		kinds = append(kinds, kind)
	}
	if len(kinds) == 0 {
		return nil, fmt.Errorf("managed_clients must declare at least one kind")
	}
	return kinds, nil
}

func parseManagedKind(path string, entry any) (managedKind, error) {
	object, ok := entry.(map[string]any)
	if !ok {
		return managedKind{}, fmt.Errorf("%s must be a managed-client declaration", path)
	}
	fields := map[string]bool{}
	for key := range object {
		if key != "kind" && key != "client_id" && key != "ids" && key != "attributes" && key != "legacy" && key != "legacy_client_id" {
			return managedKind{}, fmt.Errorf("%s: unknown managed-client field %q", path, key)
		}
		fields[key] = true
	}
	kind := managedKind{}
	text, ok := object["kind"].(string)
	if !ok || !strings.Contains(text, "-") && !managedKindSegment.MatchString(text) || strings.Contains(text, "-") && !allManagedSegments(text) {
		return managedKind{}, fmt.Errorf("%s: kind must be lowercase words separated by hyphens", path)
	}
	kind.Kind = text
	kind.Name = managedName(text)
	clientID, ok := object["client_id"].(string)
	if !ok || clientID == "" || strings.ContainsAny(clientID, "\"`'\n\r\t") {
		return managedKind{}, fmt.Errorf("%s: client_id must be a nonempty client-name template", path)
	}
	// Each {name} of the template becomes a Go identifier in the generated code, so identityChecks holds it to the
	// declared ids, and no brace may stand outside a placeholder.
	if strings.ContainsAny(clientIDPlaceholder.ReplaceAllString(clientID, ""), "{}") {
		return managedKind{}, fmt.Errorf("%s: client_id holds a brace outside a {name} placeholder", path)
	}
	kind.ClientID = clientID
	ids, ok := object["ids"].([]any)
	if !ok || len(ids) == 0 {
		return managedKind{}, fmt.Errorf("%s: ids must list the identity parameter names", path)
	}
	for _, id := range ids {
		name, ok := id.(string)
		if !ok || !managedIDName.MatchString(name) {
			return managedKind{}, fmt.Errorf("%s: ids entries must be lowercase parameter names", path)
		}
		kind.IDs = append(kind.IDs, name)
	}
	attributes, ok := object["attributes"].([]any)
	if !ok || len(attributes) == 0 {
		return managedKind{}, fmt.Errorf("%s: attributes must list the ownership attributes", path)
	}
	if len(attributes) > 16 {
		return managedKind{}, fmt.Errorf("%s: attributes exceed the sixteen-attribute binding limit", path)
	}
	seen := map[string]bool{}
	for index, attribute := range attributes {
		pair, ok := attribute.(map[string]any)
		if !ok {
			return managedKind{}, fmt.Errorf("%s.attributes[%d] must be a key and value pair", path, index)
		}
		key, ok := pair["key"].(string)
		if !ok || !managedAttributeKey.MatchString(key) || strings.HasPrefix(key, ownershipPrefix) {
			return managedKind{}, fmt.Errorf("%s.attributes[%d].key must match %s and not start with %s", path, index, managedAttributeKey, ownershipPrefix)
		}
		value, ok := pair["value"].(string)
		if !ok || value == "" || len(value) > 1024 {
			return managedKind{}, fmt.Errorf("%s.attributes[%d].value must be a literal below the length limit or an id placeholder", path, index)
		}
		for extra := range pair {
			if extra != "key" && extra != "value" {
				return managedKind{}, fmt.Errorf("%s.attributes[%d]: unknown attribute field %q", path, index, extra)
			}
		}
		if seen[key] {
			return managedKind{}, fmt.Errorf("%s.attributes[%d].key %q is declared twice", path, index, key)
		}
		seen[key] = true
		kind.Attributes = append(kind.Attributes, managedAttribute{Key: key, Value: value, ValueExpr: strconv.Quote(value)})
	}
	if flag, ok := object["legacy"].(bool); ok {
		kind.Legacy = flag
	}
	if flag, ok := object["legacy_client_id"].(bool); ok {
		kind.LegacyClientID = flag
	}
	if kind.LegacyClientID && !kind.Legacy {
		return managedKind{}, fmt.Errorf("%s: legacy_client_id requires legacy attributes", path)
	}
	for index, attribute := range kind.Attributes {
		if match := managedPlaceholder.FindStringSubmatch(attribute.Value); match != nil {
			kind.Attributes[index].ValueExpr = match[1]
		}
	}
	if _, err := kind.identityChecks(); err != nil {
		return managedKind{}, fmt.Errorf("%s: %w", path, err)
	}
	return kind, nil
}

func allManagedSegments(text string) bool {
	for _, segment := range strings.Split(text, "-") {
		if !managedKindSegment.MatchString(segment) {
			return false
		}
	}
	return true
}

// managedName converts a hyphenated kind into its exported identifier prefix.
func managedName(kind string) string {
	var name strings.Builder
	for _, segment := range strings.Split(kind, "-") {
		name.WriteString(strings.ToUpper(segment[:1]))
		name.WriteString(segment[1:])
	}
	return name.String()
}

// identityChecks validates placeholders against the declared ids.
func (k managedKind) identityChecks() ([]string, error) {
	known := map[string]bool{}
	for _, id := range k.IDs {
		known[id] = true
	}
	var missing []string
	check := func(value string) {
		if match := managedPlaceholder.FindStringSubmatch(value); match != nil && !known[match[1]] {
			missing = append(missing, match[1])
		}
	}
	for _, match := range clientIDPlaceholder.FindAllString(k.ClientID, -1) {
		check(match)
	}
	for _, attribute := range k.Attributes {
		check(attribute.Value)
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("placeholder %s is not a declared id", strings.Join(missing, ", "))
	}
	return missing, nil
}

// TemplateParams renders the Go parameter list of the identity helpers.
func (k managedKind) TemplateParams() string {
	return strings.Join(k.IDs, ", ") + " string"
}

// TemplateArgs renders the Go argument list of the identity helpers.
func (k managedKind) TemplateArgs() string {
	return strings.Join(k.IDs, ", ")
}

// EmptyCheck renders the guard that rejects missing identity ids.
func (k managedKind) EmptyCheck() string {
	checks := make([]string, 0, len(k.IDs))
	for _, id := range k.IDs {
		checks = append(checks, id+" == \"\"")
	}
	return strings.Join(checks, " || ")
}

// ClientIDExpr renders the Go expression that composes the client name: each literal part as a quoted Go string and
// each {name} placeholder as the id parameter of that name, which parseManagedKind checked.
func (k managedKind) ClientIDExpr() string {
	var parts []string
	rest := k.ClientID
	for _, at := range clientIDPlaceholder.FindAllStringSubmatchIndex(k.ClientID, -1) {
		offset := len(k.ClientID) - len(rest)
		if literal := rest[:at[0]-offset]; literal != "" {
			parts = append(parts, strconv.Quote(literal))
		}
		parts = append(parts, k.ClientID[at[2]:at[3]])
		rest = k.ClientID[at[1]:]
	}
	if rest != "" || len(parts) == 0 {
		parts = append(parts, strconv.Quote(rest))
	}
	return strings.Join(parts, " + ")
}

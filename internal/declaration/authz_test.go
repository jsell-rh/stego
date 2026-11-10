package declaration

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
)

// td03Files reads ST-DECL-TD03: the declaration and its fixture schema.
func td03Files(t *testing.T) (string, string) {
	t.Helper()
	service, err := os.ReadFile("testdata/td03/service.yaml")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile("testdata/td03/authz/schema.zed")
	if err != nil {
		t.Fatal(err)
	}
	return string(service), string(schema)
}

// change replaces the first match of old in text, which must hold it.
func change(t *testing.T, text, old, new string) string {
	t.Helper()
	if !strings.Contains(text, old) {
		t.Fatalf("the fixture does not hold %q", old)
	}
	return strings.Replace(text, old, new, 1)
}

// readTD03 reads a declaration as a value; the declaration must hold no undeclared field.
func readTD03(t *testing.T, service string) *types.ServiceDeclaration {
	t.Helper()
	decl, undeclared, err := parser.ReadServiceDeclaration([]byte(service), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %v", err, undeclared)
	}
	return decl
}

// checkTD03 runs checking in process on the declaration and the schema: the declaration's own checks and the
// authorization checks, with the schema at authz/schema.zed of the project folder.
func checkTD03(t *testing.T, service, schema string) []ValidationError {
	t.Helper()
	decl := readTD03(t, service)
	project := fstest.MapFS{"authz/schema.zed": {Data: []byte(schema)}}
	faults := Validate(decl).All()
	return append(faults, CheckAuthorization(decl, project)...)
}

// compileTD03 compiles the schema, which must compile.
func compileTD03(t *testing.T, schema string) *AuthzSchema {
	t.Helper()
	compiled, err := CompileAuthzSchema("authz/schema.zed", []byte(schema))
	if err != nil {
		t.Fatalf("the fixture schema does not compile: %v", err)
	}
	return compiled
}

// expectOneFault expects exactly one fault of the rule, and that it names each text in its found value, path or
// message.
func expectOneFault(t *testing.T, faults []ValidationError, rule string, texts ...string) ValidationError {
	t.Helper()
	var matched []ValidationError
	for _, fault := range faults {
		if fault.Rule == rule {
			matched = append(matched, fault)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("want one fault of %s, got %+v", rule, faults)
	}
	whole := matched[0].Found + " " + matched[0].Path + " " + matched[0].Message
	for _, text := range texts {
		if !strings.Contains(whole, text) {
			t.Errorf("the fault does not name %q: %+v", text, matched[0])
		}
	}
	return matched[0]
}

func expectAccepted(t *testing.T, faults []ValidationError) {
	t.Helper()
	if len(faults) != 0 {
		t.Fatalf("want no fault, got %+v", faults)
	}
}

// withBinding adds a slot binding to the declaration.
func withBinding(service, binding string) string {
	return service + "slots:\n" + binding
}

func TestSchemaNames(t *testing.T) {
	service, schema := td03Files(t)
	t.Run("ST-DECL-INV22-T A schema that does not compile, and a name the schema lacks, are refused", func(t *testing.T) {
		t.Run("syntax error on line 4", func(t *testing.T) {
			broken := change(t, schema, "\trelation viewer: user\n}", "\trelation viewer user\n}")
			fault := expectOneFault(t, checkTD03(t, service, broken), "schema-compile", "authz/schema.zed")
			if fault.Found != "authz/schema.zed:4" || !strings.Contains(fault.Message, "line 4") {
				t.Errorf("the schema fault does not name line 4: %+v", fault)
			}
		})
		cases := []struct{ name, old, new, found, kind, path string }{
			{"resource type archive", "resource_type: document", "resource_type: archive", "archive", "resource type", "entities[0].resource_type"},
			{"permission destroy", "delete: edit", "delete: destroy", "destroy", "permission", "entities[0].permissions.delete"},
			{"relation approver", "{relation: owner,", "{relation: approver,", "approver", "relation", "entities[0].relationships[0].relation"},
			{"context field request.region", "    relationships:\n", "    context_fields: [request.region]\n    relationships:\n", "request.region", "caveat parameter", "entities[0].context_fields[0]"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				fault := expectOneFault(t, checkTD03(t, change(t, service, c.old, c.new), schema), "schema-names", c.found, c.kind)
				if fault.Found != c.found || fault.Path != c.path {
					t.Errorf("want the fault at %s naming %s, got %+v", c.path, c.found, fault)
				}
			})
		}
		t.Run("TD03 unchanged", func(t *testing.T) {
			expectAccepted(t, checkTD03(t, service, schema))
		})
	})

	t.Run("ST-DECL-SP05-T A resource type, permission, relation or caveat parameter the compiled schema lacks each satisfy it", func(t *testing.T) {
		compiled := compileTD03(t, schema)
		binding := "  - {collection: todos, slot: after_create, chain: [relate], relationships_written: [document#approver]}\n"
		satisfying := map[string]string{
			"resource type archive":        change(t, service, "resource_type: document", "resource_type: archive"),
			"permission destroy":           change(t, service, "delete: edit", "delete: destroy"),
			"binding names approver":       withBinding(service, binding),
			"context field request.region": change(t, service, "    relationships:\n", "    context_fields: [request.region]\n    relationships:\n"),
		}
		for name, changed := range satisfying {
			if !SchemaLacksName(readTD03(t, changed), compiled) {
				t.Errorf("%s does not satisfy the predicate", name)
			}
		}
		if SchemaLacksName(readTD03(t, service), compiled) {
			t.Error("TD03 unchanged satisfies the predicate")
		}
	})
}

func TestWrittenRelations(t *testing.T) {
	service, schema := td03Files(t)
	binding := func(written string) string {
		return withBinding(service, "  - {collection: todos, slot: after_create, chain: [relate], relationships_written: ["+written+"]}\n")
	}
	t.Run("ST-DECL-INV23-T A slot binding that may write a relation the schema lacks, or one of a type no entity uses, is refused", func(t *testing.T) {
		t.Run("document#viewer", func(t *testing.T) {
			expectAccepted(t, checkTD03(t, binding("document#viewer"), schema))
		})
		for _, c := range []struct{ written, reason string }{
			{"document#approver", "lacks the relation approver"},
			{"folder#viewer", "the resource type of no entity"},
		} {
			t.Run(c.written, func(t *testing.T) {
				fault := expectOneFault(t, checkTD03(t, binding(c.written), schema), "written-relations", c.written, "todos", "after_create", c.reason)
				if fault.Path != "slots[0].relationships_written[0]" {
					t.Errorf("the fault is not at the binding's relation: %+v", fault)
				}
			})
		}
	})

	t.Run("ST-DECL-SP06-T A written relation the schema lacks, or of a type no entity declares, satisfies it", func(t *testing.T) {
		compiled := compileTD03(t, schema)
		for written, want := range map[string]bool{"document#approver": true, "folder#viewer": true, "document#viewer": false} {
			if got := SlotRelationUnknown(readTD03(t, binding(written)), compiled); got != want {
				t.Errorf("%s: the predicate gives %v, want %v", written, got, want)
			}
		}
	})
}

func TestAuthorizationCoverage(t *testing.T) {
	service, schema := td03Files(t)
	publicList := func(reason string) string {
		changed := change(t, service, "list: view}", "list: public}")
		if reason != "" {
			changed = change(t, changed, "    relationships:\n", "    public_reasons: {list: "+reason+"}\n    relationships:\n")
		}
		return changed
	}
	td02 := change(t, change(t, change(t, change(t, service,
		"authorization:\n  provider: spicedb\n  schema_path: authz/schema.zed\n  subject_from_identity: user\n", ""),
		"    resource_type: document\n", ""),
		"    permissions: {create: edit, read: view, update: edit, delete: edit, list: view}\n", ""),
		"    relationships:\n      - {relation: owner, subject_source: field, subject_field: owner_id, subject_type: user}\n", "")

	t.Run("ST-DECL-INV31-T With authorization bound, an entity with no resource type and an operation with no permission are refused, each named", func(t *testing.T) {
		coverage := func(t *testing.T, changed string) []ValidationError {
			t.Helper()
			var faults []ValidationError
			for _, fault := range checkTD03(t, changed, schema) {
				if fault.Rule == "authorization-coverage" {
					faults = append(faults, fault)
				}
			}
			return faults
		}
		t.Run("Todo with no resource type", func(t *testing.T) {
			noType := change(t, change(t, service, "    resource_type: document\n", ""), "    relationships:\n      - {relation: owner, subject_source: field, subject_field: owner_id, subject_type: user}\n", "")
			expectOneFault(t, coverage(t, noType), "authorization-coverage", "Todo")
		})
		t.Run("Tag with no resource type", func(t *testing.T) {
			tag := change(t, service, "collections:\n", "  - name: Tag\n    fields:\n      - {name: label, type: string}\ncollections:\n") + "  tags:\n    entity: Tag\n    operations: [read]\n"
			fault := expectOneFault(t, coverage(t, tag), "authorization-coverage", "Tag")
			if strings.Contains(fault.Message, `"Todo"`) {
				t.Errorf("the fault names Todo, which has its resource type: %+v", fault)
			}
		})
		t.Run("no permission for list", func(t *testing.T) {
			expectOneFault(t, coverage(t, change(t, service, ", list: view}", "}")), "authorization-coverage", "todos", "list")
		})
		t.Run("no permission for update and delete", func(t *testing.T) {
			faults := coverage(t, change(t, service, "update: edit, delete: edit, ", ""))
			if len(faults) != 2 || !strings.Contains(faults[0].Found, "todos.update") || !strings.Contains(faults[1].Found, "todos.delete") {
				t.Fatalf("want two faults, todos with update and todos with delete, got %+v", faults)
			}
		})
		t.Run("list public with no reason", func(t *testing.T) {
			expectOneFault(t, coverage(t, publicList("")), "authorization-coverage", "todos", "list", "public", "no reason")
		})
		t.Run("list public with a reason", func(t *testing.T) {
			expectAccepted(t, checkTD03(t, publicList("the catalog is public by design"), schema))
		})
		t.Run("TD02", func(t *testing.T) {
			expectAccepted(t, checkTD03(t, td02, schema))
		})
	})

	t.Run("ST-DECL-SP18-T With authorization bound, an entity with no resource type or an operation with no permission satisfies it", func(t *testing.T) {
		for name, c := range map[string]struct {
			service string
			want    bool
		}{
			"Todo with no resource type":   {change(t, service, "    resource_type: document\n", ""), true},
			"no permission for list":       {change(t, service, ", list: view}", "}"), true},
			"list public with no reason":   {publicList(""), true},
			"list public with a reason":    {publicList("the catalog is public by design"), false},
			"TD03 unchanged":               {service, false},
			"TD02, no authorization bound": {td02, false},
		} {
			if got := operationUnprotected(readTD03(t, c.service)); got != c.want {
				t.Errorf("%s: the predicate gives %v, want %v", name, got, c.want)
			}
		}
	})
}

func TestAuthorizationTenancy(t *testing.T) {
	t.Run("ST-DECL-INV84-T A declaration that binds authorization without pooled tenancy is refused", func(t *testing.T) {
		service, schema := td03Files(t)
		expectAccepted(t, checkTD03(t, service, schema))
		block := "tenancy:\n  kind: pooled\n  key: tenant_id\n  claim: tenant\n"
		omitted := change(t, service, block, "")
		expectOneFault(t, checkTD03(t, omitted, schema), "authorization-tenancy", "tenancy", "tenant-qualified", "pooled")
		single := change(t, service, block, "tenancy:\n  kind: single\n  reason: one customer per deployment\n")
		expectOneFault(t, checkTD03(t, single, schema), "authorization-tenancy", "tenancy.kind", "single", "pooled")
		// A declaration that binds no authorization needs no tenancy.
		unbound := withoutAuthorizationParts(t, omitted)
		for _, fault := range checkTD03(t, unbound, schema) {
			if fault.Rule == "authorization-tenancy" {
				t.Errorf("a declaration without authorization gets %+v", fault)
			}
		}
	})
}

// withoutAuthorizationParts is a declaration of ST-DECL-TD03 with its authorization setting and the authorization
// parts of Todo left out.
func withoutAuthorizationParts(t *testing.T, service string) string {
	t.Helper()
	service = change(t, service, "authorization:\n  provider: spicedb\n  schema_path: authz/schema.zed\n  subject_from_identity: user\n", "")
	var kept []string
	skip := false
	for _, line := range strings.Split(service, "\n") {
		switch {
		case strings.HasPrefix(line, "    resource_type:"), strings.HasPrefix(line, "    permissions:"):
			continue
		case strings.HasPrefix(line, "    relationships:"):
			skip = true
			continue
		case skip && strings.HasPrefix(line, "      - "):
			continue
		}
		skip = false
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

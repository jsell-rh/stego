package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
)

// userManagementBase reads the declaration of ST-GEN-TD01, examples/user-management, and a record that names it.
func userManagementBase(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "user-management", "service.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func recordFor(declaration []byte) []byte {
	return []byte("format_version: 5\nlast_applied:\n  service_hash: " + HashBytes(declaration) + "\n")
}

func parseHead(t *testing.T, data string) *types.ServiceDeclaration {
	t.Helper()
	head, err := parser.ParseServiceDeclarationFromBytes([]byte(data), "service.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// withChanges appends schema changes and schema history to a declaration.
func withChanges(declaration, changes, history string) string {
	return strings.TrimRight(declaration, "\n") + "\nschema_changes:\n" + changes + "schema_history:\n" + history
}

const (
	emailRename       = "  - {id: user-email-rename, kind: rename, entity: User, field: contact_email, from: email}\n"
	teamAdd           = "  - {id: add-team, kind: add, entity: Team}\n"
	nicknameAdd       = "  - {id: add-user-nickname, kind: add, entity: User, field: nickname}\n"
	emailRenameExpand = "  - {change: user-email-rename, phase: expand}\n"
	teamAddExpand     = "  - {change: add-team, phase: expand}\n"
	nicknameAddExpand = "  - {change: add-user-nickname, phase: expand}\n"
	team              = "  - name: Team\n    resource_type: team\n    permissions: {read: view, list: view}\n    fields:\n      - { name: label, type: string }\n\n"
	nickname          = "      - { name: nickname, type: string, optional: true }\n"
)

func renameEmail(declaration string) string {
	return strings.Replace(declaration, "{ name: email, type: string", "{ name: contact_email, type: string", 1)
}

func addTeam(declaration string) string {
	return strings.Replace(declaration, "\ncollections:\n", "\n"+team+"collections:\n", 1)
}

func addNickname(declaration string) string {
	return strings.Replace(declaration, "      - { name: metadata, type: jsonb, optional: true }\n", "      - { name: metadata, type: jsonb, optional: true }\n"+nickname, 1)
}

func TestBaseCompat(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-INV33-T An undeclared change to a base entity is refused, and a declared one that moves one phase is accepted", func(t *testing.T) {
		t.Parallel()
		td01 := string(userManagementBase(t))
		// A base with two finished-or-open schema steps, so that the order and the removal of the base's steps
		// can be tested.
		stepped := withChanges(addNickname(addTeam(td01)), teamAdd+nicknameAdd, teamAddExpand+nicknameAddExpand)
		cases := []struct {
			name, base, head string
			refused          bool
			entity, says     string
		}{
			{"undeclared rename", td01, renameEmail(td01), true, "User", "renames User.email to contact_email"},
			{"declared rename at expand", td01, withChanges(renameEmail(td01), emailRename, emailRenameExpand), false, "", ""},
			{"undeclared new entity", td01, addTeam(td01), true, "Team", "adds the entity Team"},
			{"declared add of the new entity", td01, withChanges(addTeam(td01), teamAdd, teamAddExpand), false, "", ""},
			{"base steps in another order", stepped, withChanges(addNickname(addTeam(td01)), teamAdd+nicknameAdd, nicknameAddExpand+teamAddExpand), true, "Team", "must start with the base's steps in the same order"},
			{"base schema change removed", stepped, withChanges(addNickname(addTeam(td01)), teamAdd, teamAddExpand), true, "User", "removes the schema change add-user-nickname"},
			{"expand and contract in one release", td01, withChanges(renameEmail(td01), emailRename, emailRenameExpand+"  - {change: user-email-rename, phase: contract}\n"), true, "User", "its expand and its contract step in one release"},
		}
		for _, c := range cases {
			base := []byte(c.base)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, c.head))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if !c.refused {
				if check.HasFaults() {
					t.Errorf("%s: want accepted, got %+v", c.name, check.Faults)
				}
				continue
			}
			if !check.HasFaults() {
				t.Errorf("%s: want refused, got no fault", c.name)
				continue
			}
			named := false
			for _, fault := range check.Faults {
				if fault.Rule != BaseCompatRule {
					t.Errorf("%s: fault %+v names the rule %q, want %s", c.name, fault, fault.Rule, BaseCompatRule)
				}
				named = named || (fault.Entity == c.entity && strings.Contains(fault.Message, c.says))
			}
			if !named {
				t.Errorf("%s: no fault names the entity %s and says %q: %+v", c.name, c.entity, c.says, check.Faults)
			}
		}
	})
}

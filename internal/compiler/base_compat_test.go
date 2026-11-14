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
	t.Run("ST-GEN-INV33-T A change to what a step of the base added, required or retyped, or to a unique invariant or an entity's table settings, is refused", func(t *testing.T) {
		t.Parallel()
		td01 := string(userManagementBase(t))
		const metadata = "      - { name: metadata, type: jsonb, optional: true }\n"
		added := withChanges(addNickname(td01), nicknameAdd, nicknameAddExpand)
		withInvariant := strings.TrimRight(td01, "\n") + "\ninvariants:\n  - {name: user-email-org, kind: unique, on: User, by: [email, org_id]}\n"
		retyped := withChanges(strings.Replace(td01, "{ name: display_name, type: string, min_length: 1, max_length: 100 }", "{ name: display_name, type: int64, optional: true }", 1),
			"  - {id: user-display-name-retype, kind: retype, entity: User, field: display_name, from: string, conversion: text_to_int64, inverse_conversion: int64_to_text}\n",
			"  - {change: user-display-name-retype, phase: expand}\n")
		required := withChanges(strings.Replace(td01, metadata, metadata+"      - { name: priority, type: int64 }\n", 1),
			"  - {id: user-priority-require, kind: require, entity: User, field: priority, fill_value: 0}\n",
			"  - {change: user-priority-require, phase: expand}\n")
		teamAdded := withChanges(addTeam(td01), teamAdd, teamAddExpand)
		cases := []struct{ name, base, head, says string }{
			{"retype of an added field", added, strings.Replace(added, nickname, "      - { name: nickname, type: int64, optional: true }\n", 1), "changes the stored shape of User.nickname"},
			{"added field made required with no default", added, strings.Replace(added, nickname, "      - { name: nickname, type: string }\n", 1), "changes the stored shape of User.nickname"},
			{"added field with a narrower max_length", added, strings.Replace(added, nickname, "      - { name: nickname, type: string, optional: true, max_length: 3 }\n", 1), "changes the stored shape of User.nickname"},
			{"added field made unique", added, strings.Replace(added, nickname, "      - { name: nickname, type: string, optional: true, unique: true }\n", 1), "changes the stored shape of User.nickname"},
			{"unique invariant changed under its name", withInvariant, strings.Replace(withInvariant, "by: [email, org_id]", "by: [display_name]", 1), "changes the invariant user-email-org"},
			{"retyped field narrowed and made required", retyped, strings.Replace(retyped, "{ name: display_name, type: int64, optional: true }", "{ name: display_name, type: int64, max: 2, unique: true }", 1), "changes the stored shape of User.display_name"},
			{"required field retyped", required, strings.Replace(required, "{ name: priority, type: int64 }", "{ name: priority, type: string, max_length: 1 }", 1), "changes the stored shape of User.priority"},
			{"field of an added entity retyped", teamAdded, strings.Replace(teamAdded, "{ name: label, type: string }", "{ name: label, type: int64, unique: true }", 1), "changes the stored shape of Team.label"},
			{"declared add of a unique field with a default", td01, withChanges(strings.Replace(td01, metadata, metadata+"      - { name: code, type: string, unique: true, default: x }\n", 1), "  - {id: add-user-code, kind: add, entity: User, field: code}\n", "  - {change: add-user-code, phase: expand}\n"), "both unique and a default"},
			{"entity made versioned", td01, strings.Replace(td01, "  - name: User\n", "  - name: User\n    versioned: true\n", 1), "changes the table settings of User"},
			{"base unique invariant removed", withInvariant, td01, "removes or changes the invariant user-email-org"},
			{"new unique change takes over a base invariant's name", withInvariant, withChanges(strings.Replace(withInvariant, "by: [email, org_id]", "by: [display_name]", 1), "  - {id: user-email-org-unique, kind: unique, entity: User, field: user-email-org}\n", "  - {change: user-email-org-unique, phase: expand}\n"), "names user-email-org, an invariant that the base declares"},
			{"declared add of a field unique when live with a default", td01, withChanges(strings.Replace(td01, metadata, metadata+"      - { name: code, type: string, unique_when_live: true, default: x }\n", 1), "  - {id: add-user-code, kind: add, entity: User, field: code}\n", "  - {change: add-user-code, phase: expand}\n"), "both unique and a default"},
			{"constraints of a base field changed with its type kept", td01, strings.Replace(td01, "{ name: display_name, type: string, min_length: 1, max_length: 100 }", "{ name: display_name, type: string, max_length: 5, unique: true }", 1), "keeps its type"},
			{"entity tenant changed", td01, strings.Replace(td01, "  - name: OrgSetting\n", "  - name: OrgSetting\n    tenant: shared\n", 1), "changes the table settings of OrgSetting"},
		}
		for _, c := range cases {
			if c.head == c.base {
				t.Fatalf("%s: the head is the base", c.name)
			}
			base := []byte(c.base)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, c.head))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			found := false
			for _, fault := range check.Faults {
				found = found || (fault.Rule == BaseCompatRule && strings.Contains(fault.Message, c.says))
			}
			if !found {
				t.Errorf("%s: no fault says %q: %+v", c.name, c.says, check.Faults)
			}
		}
		for _, c := range []struct{ name, base, head string }{
			{"the base itself, with its add", added, added},
			{"the base with its retype", retyped, retyped},
			{"a new add after the base's add", added, withChanges(addNickname(addTeam(td01)), nicknameAdd+teamAdd, nicknameAddExpand+teamAddExpand)},
		} {
			base := []byte(c.base)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, c.head))
			if err != nil || check.HasFaults() {
				t.Errorf("%s: want accepted, got %v %+v", c.name, err, check.Faults)
			}
		}
	})
}

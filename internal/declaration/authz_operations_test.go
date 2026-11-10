package declaration

import (
	"strings"
	"testing"
	"testing/fstest"
)

// operationFacts are the facts of the TD03 operations: a fill of the command slot, of the query slot and of the
// worker slot, and the outbox destination billing that an active component declares.
func operationFacts() Facts {
	return Facts{
		FillSlots:    map[string]string{"reserve-and-charge": CommandSlot, "open-reservations": QuerySlot, "sync": WorkerSlot, "apply": WorkerSlot},
		Destinations: map[string]bool{"billing": true},
	}
}

// checkOperations runs checking in process on a declaration of ST-DECL-TD03 as stego validate does: the
// declaration's own checks, its composition with the facts given, and its authorization against the fixture
// schema. It gives every fault.
func checkOperations(t *testing.T, service string, facts Facts) []ValidationError {
	t.Helper()
	_, schema := td03Files(t)
	decl := readTD03(t, service)
	faults := Validate(decl).All()
	faults = append(faults, ValidateComposition(decl, facts)...)
	return append(faults, CheckAuthorization(decl, fstest.MapFS{"authz/schema.zed": {Data: []byte(schema)}})...)
}

// expectNamed expects exactly one fault of the rule, and that its value found, path or message names each text.
func expectNamed(t *testing.T, faults []ValidationError, rule string, texts ...string) {
	t.Helper()
	expectOneFault(t, faults, rule, texts...)
}

// td03Command is ST-DECL-TD03 with one command that writes Todo, bound to the fill reserve-and-charge, with the
// lines given.
func td03Command(t *testing.T, lines ...string) string {
	service, _ := td03Files(t)
	return service + "commands:\n" + standardCommand("approve", lines...)
}

func TestCommandAuthorization(t *testing.T) {
	full := []string{"resource_type: document", "permission: edit", "resource_member: documentId"}
	without := func(drop string) []string {
		var kept []string
		for _, line := range full {
			if !strings.HasPrefix(line, drop+":") {
				kept = append(kept, line)
			}
		}
		return kept
	}
	t.Run("ST-DECL-INV40-T A command on document with edit and a resource member is accepted", func(t *testing.T) {
		expectAccepted(t, checkOperations(t, td03Command(t, full...), operationFacts()))
	})
	t.Run("ST-DECL-INV40-T A command with no permission is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Command(t, without("permission")...), operationFacts()), "command-authorization", "approve", "no permission")
	})
	t.Run("ST-DECL-INV40-T A command with no resource type is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Command(t, without("resource_type")...), operationFacts()), "command-authorization", "approve", "no resource type")
	})
	t.Run("ST-DECL-INV40-T A command with no resource member is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Command(t, without("resource_member")...), operationFacts()), "command-authorization", "approve", "no resource member")
	})
	t.Run("ST-DECL-INV40-T A command with the permission approve, which the schema lacks, is refused", func(t *testing.T) {
		lines := append(without("permission"), "permission: approve")
		expectNamed(t, checkOperations(t, td03Command(t, lines...), operationFacts()), "command-authorization", "approve", "permission \"approve\"")
	})

	relations := func(relation string) string {
		return td03Command(t, append(full, "relationships_written: ["+relation+"]")...)
	}
	t.Run("ST-DECL-INV41-T A command that writes document#viewer is accepted", func(t *testing.T) {
		expectAccepted(t, checkOperations(t, relations("document#viewer"), operationFacts()))
	})
	t.Run("ST-DECL-INV41-T A command that writes document#approver, which the schema lacks, is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, relations("document#approver"), operationFacts()), "command-relations", "approve", "document#approver", "lacks the relation approver")
	})
	t.Run("ST-DECL-INV41-T A command that writes folder#viewer, of no entity the command writes, is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, relations("folder#viewer"), operationFacts()), "command-relations", "approve", "folder#viewer", "no entity the command writes")
	})
}

// td03Query is ST-DECL-TD03 with one query that reads Todo, bound to the fill open-reservations, with the lines
// given.
func td03Query(t *testing.T, lines ...string) string {
	service, _ := td03Files(t)
	return service + "queries:\n" + standardQuery("open-documents", lines...)
}

func TestQueryAuthorization(t *testing.T) {
	t.Run("ST-DECL-INV46-T A query on document with view is accepted", func(t *testing.T) {
		expectAccepted(t, checkOperations(t, td03Query(t, "resource_type: document", "permission: view", "resource_member: documentId"), operationFacts()))
	})
	t.Run("ST-DECL-INV46-T A query with no permission is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Query(t, "resource_type: document"), operationFacts()), "query-authorization", "open-documents", "no permission")
	})
	t.Run("ST-DECL-INV46-T A query with no resource type is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Query(t, "permission: view"), operationFacts()), "query-authorization", "open-documents", "no resource type")
	})
	t.Run("ST-DECL-INV46-T A query with the permission audit, which the schema lacks, is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Query(t, "resource_type: document", "permission: audit"), operationFacts()), "query-authorization", "open-documents", "permission \"audit\"")
	})
	t.Run("ST-DECL-INV46-T A list query with view and no resource member is accepted", func(t *testing.T) {
		expectAccepted(t, checkOperations(t, td03Query(t, "resource_type: document", "permission: view"), operationFacts()))
	})
}

// td03Worker is ST-DECL-TD03 with the modes api and worker and the worker sync-documents with the trigger every
// 1m that writes Todo, bound to the fill sync, with the lines given.
func td03Worker(t *testing.T, lines ...string) string {
	service, _ := td03Files(t)
	return service + "modes: [api, worker]\nworkers:\n" + worker("sync-documents", append([]string{"trigger: every 1m", "writes: [Todo]", "fill: sync"}, lines...)...)
}

func TestWorkerAuthorization(t *testing.T) {
	t.Run("ST-DECL-INV69-T The worker sync-documents on document with view is accepted", func(t *testing.T) {
		expectAccepted(t, checkOperations(t, td03Worker(t, "resource_type: document", "permission: view"), operationFacts()))
	})
	t.Run("ST-DECL-INV69-T A worker with no permission is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Worker(t, "resource_type: document"), operationFacts()), "worker-authorization", "sync-documents", "no permission")
	})
	t.Run("ST-DECL-INV69-T A worker with no resource type is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Worker(t, "permission: view"), operationFacts()), "worker-authorization", "sync-documents", "no resource type")
	})
	t.Run("ST-DECL-INV69-T A worker with the permission approve, which the schema lacks, is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Worker(t, "resource_type: document", "permission: approve"), operationFacts()), "worker-authorization", "sync-documents", "permission \"approve\"")
	})
	t.Run("ST-DECL-INV69-T A worker with the resource type invoice, which the schema lacks, is refused", func(t *testing.T) {
		expectNamed(t, checkOperations(t, td03Worker(t, "resource_type: invoice", "permission: view"), operationFacts()), "worker-authorization", "sync-documents", "resource type \"invoice\"")
	})
}

// replyWorker is the worker apply-document-replies with the trigger consumes document-replies that writes Todo on
// the resource type document with the permission edit, bound to the fill apply.
func replyWorker() string {
	return worker("apply-document-replies", "trigger: consumes document-replies", "writes: [Todo]", "fill: apply", "resource_type: document", "permission: edit")
}

// td03Staging is ST-DECL-TD03 with the modes api and worker, the worker apply-document-replies and the items given.
func td03Staging(t *testing.T, workers string, commands string) string {
	service, _ := td03Files(t)
	service += "modes: [api, worker]\nworkers:\n" + replyWorker() + workers
	if commands != "" {
		service += "commands:\n" + commands
	}
	return service
}

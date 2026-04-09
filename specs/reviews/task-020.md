# Review: Task 020 — Update Example Service and README for Collections

## Round 1

No findings. All 7 acceptance criteria verified:

- [x] AC1: `examples/user-management/service.yaml` uses `collections:` with 3 collections (`organizations`, `org-users`, `all-users`), two referencing the same entity (`User`)
- [x] AC2: Slot bindings use `collection: org-users`, not `entity:`
- [x] AC3: All 4 fill YAML files use `collection: org-users`, not `entity:`
- [x] AC4: `stego validate` passes on the updated service.yaml
- [x] AC5: `stego apply` produces output that compiles with `go build ./...`; `stego apply` reports "No changes. Infrastructure is up-to-date." confirming checked-in output is current
- [x] AC6: Generated `main.go` wires fills to collection-scoped handlers: `beforeCreateOrgUsersGate` and `onEntityChangedOrgUsersFanOut` passed to `api.NewOrgUsersHandler`; `AllUsersHandler` and `OrganizationsHandler` have no slot wiring (correct — no slots bound to those collections)
- [x] AC7: README lists "Six nouns" including **Collection** (line 110) and uses `collections:` format in Quick Start example (lines 72-76); `stego fill create` example shows `-collection` flag (line 90)

Additional verification:
- [x] All tests pass (`go test ./...` — 13 packages)
- [x] OpenAPI spec uses collection-derived operation IDs (`createOrganizations`, `listOrgUsers`, `listAllUsers`) and tags (`organizations`, `org-users`, `all-users`)
- [x] Handler files named per collection (`handler_organizations.go`, `handler_org_users.go`, `handler_all_users.go`)
- [x] Routes match spec: `/organizations`, `/organizations/{org_id}/users`, `/users`
- [x] Scoped list (`org-users`) filters by `org_id`; unscoped list (`all-users`) returns all

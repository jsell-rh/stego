# Repository instructions

Use ASD-STE100 Simplified Technical English for prose.
Commit work in atomic commits.

Playwright is allowed. The kernel panic it once caused on the developer
workstation is resolved (2026-11-05).
Local tests never need a privileged container: a test that needs a Kubernetes
API uses envtest (kube-apiserver and etcd as plain processes) and contract
fakes for Tekton and Argo CD; a run of real pods is a second proof in a
cluster, never the only one (ST-STD-LOCAL).
Do not run performance or stress tests on the developer workstation. Use CI or
the OpenShift jshell cluster. Use the saved jshell context explicitly; do not
change the active context. Keep cluster test resources in a dedicated namespace
with CPU, memory, and time limits. Do not use privileged containers.

Keep ordinary local checks small. Run heavy test suites in CI or the cluster.
If a test is interrupted, inspect its process or job before starting another run.
A missing result is not a pass. Preserve unrelated files and workloads.

Do not edit a shell script while it is running. Use a frozen source copy if
source edits must continue during a check.

Keep frozen test sources, operator journals, and result records in persistent
storage. Do not use `/tmp` for their only copies.

## STEGO's own code

The specification under `data/` is the contract. STEGO's own code follows
the standard ST-STD-CODE in it, which `lathe check` and `assure gate change`
hold the code to:

- One domain per Go package under `internal` and `cmd`; imports between
  domains follow the order in ST-STD-CODE-R03.
- A technical record's title is the identifier its file declares.
- Every source file is named by a technical record. Every test carries the
  ID of the test record it proves in its name.
- A function a record names reads its inputs. A stub is not built.

Change the technical stage of the specification (files, identifiers, tables,
test records) with the Lathe CLI in the same change as the code. Do not
change design records (requirements, invariants, flows, contracts): write a
`question` record for the Product owner instead, and stop that use case.
Make one commit per use case, or per set of gaps closed together.

# Repository instructions

Use ASD-STE100 Simplified Technical English for prose.
Commit work in atomic commits.

Do not use Playwright. The user reports that it can cause a kernel panic.
Do not run performance or stress tests on the developer workstation. Use CI or
the OpenShift jshell cluster. Use the saved jshell context explicitly; do not
change the active context. Keep cluster test resources in a dedicated namespace
with CPU, memory, and time limits. Do not use privileged containers.

Keep ordinary local checks small. Run heavy test suites in CI or the cluster.
If a test is interrupted, inspect its process or job before starting another run.
A missing result is not a pass. Preserve unrelated files and workloads.

Do not edit a shell script while it is running. Use a frozen source copy if
source edits must continue during a check.

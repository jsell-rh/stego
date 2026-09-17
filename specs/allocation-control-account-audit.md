# Control account reuse audit

The generated allocator ClusterRoleBinding and control-worker RoleBindings use
fixed ServiceAccount names in the control namespace. The workload account guards
do not clearly cover those names. Test this boundary before the next component
release. No exploit is confirmed by the source review alone.

The runner `scripts/check-allocation-control-identity.py` uses the related and
peer manifests from the allocation account artifact. It keeps the generated
allocator binding and a declared worker binding while it replaces the control
namespace. A separate fixture identity creates the replacement namespace. It
receives only account and token creation rights in that namespace.

The test verifies both account names. It first proves that the original tokens
can use their grants and the ordinary owner token cannot. After replacement,
it requires the old tokens to be rejected. It then checks account creation by
the new owner and uses fresh tokens to test the retained grants. Token values
stay in memory and do not enter process arguments or evidence files. TLS uses
the saved cluster CA, without redirects or a proxy.

If a fresh account token obtains retained access, the security gate fails. Both
account results are retained before that failure. An unrelated admission error,
a transport error, or a missing response cannot establish a denial. This runner
is prepared for a live check; it does not establish a safe boundary.

Use the shared test lease after the Hypershell browser and API tests finish and
cleanup passes. The runner creates no Pods. It retains exact resource UIDs and
uses the common 600-second execution and 180-second cleanup limits. Cleanup
removes allocated namespaces before their guards and preserves any unrecorded
replacement resource. Eight local runner safety checks passed. Live results are
still required.

[Hosted run 35252021419](https://github.com/jsell-rh/stego/actions/runs/35252021419)
passed at `62841ce`. Independent checks matched the source archive, all five
generator checks, 13 generated runtime tests with 37 cases, and 23 runner safety
checks. All three manifests match the earlier qualified fixture. These results
qualify the test inputs; they do not prove the live control-account boundary.
See the [recorded evidence](allocation-control-account-audit-evidence.json).

The full check also passed at `62841ce` in
[run 35252021322](https://github.com/jsell-rh/stego/actions/runs/35252021322).
All six jobs passed. The saved logs confirm 34 race-test packages, the required
SQL credential and browser checks, and both generated examples. The live audit
is still blocked by the application test and cleanup prerequisites.

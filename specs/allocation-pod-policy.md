# Pod restrictions for allocated namespaces

An allocation profile can require `pod_runtime_class`, `pod_service_account`, or
both. The runtime value is an exact RuntimeClass name. The account value is a
local alias from that profile's `service_accounts`. It cannot select a literal
account name, the default account, or an account imported from another profile.
The normal requirement for a declared account binding still applies.

STEGO generates one ValidatingAdmissionPolicy and one denying binding for each
profile with these settings. The policy selects the immutable allocator and
profile labels. It requires current allocation identity and restricted Pod
security. The account rule compares the Pod with the allocation's sealed account
name and requires explicit `automountServiceAccountToken: false`. The runtime
rule rejects a missing or different RuntimeClass. These checks apply to Pod
creation, Pod updates, and the ephemeral-container update path. Pod deletion
remains available.

Install the generated policy before granting allocator and workload permissions.
Keep its policy and binding outside worker write access. A policy update applies
to subsequent admission requests; it does not stop existing Pods. Plan runtime
changes with that limit. The declaration does not install a RuntimeClass or
prove the isolation of its handler. The operator must verify that runtime.

The fields add no permissions and do not relax the immutable restricted Pod
security level. Profiles without the fields retain their existing output. The
worker does not interpret these settings; enforcement belongs to Kubernetes
admission. See the [admission reference](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/)
and [RuntimeClass reference](https://kubernetes.io/docs/concepts/containers/runtime-class/).

The [focused CI run](https://github.com/jsell-rh/stego/actions/runs/35265424651)
passed three generator checks, 18 invalid-input cases, and 15 runner safety
checks at `fe4e88b`. Independent checks matched the source archive and both
rendered installations. The [full CI run](https://github.com/jsell-rh/stego/actions/runs/35265248026)
passed all six jobs, including 34 packages in the race suite, at generator source
`e5f8c65`. Later changes add the live runner and documentation only.

The jshell check passed on 2026-09-17 at runner source `fe4e88b`. It checked all
16 policies in two installations and passed 11 server dry-run Pod requests.
These include allowed requests, denied runtime and account changes, token-mount
denials, label changes, and access to an unrelated namespace. The writer also
could not create accounts, change admission policy, or create RuntimeClasses.
No Pod was stored and no runtime handler was installed. Independent reads
confirmed that all test resources were absent. The shared test lease was
released after cleanup.

The result and cleanup records are retained under
`~/.local/state/stego/runs/allocation-pod-policy-20260917/`. These checks prove
admission and namespace permissions within the test scope. They do not prove
VM isolation. Hypershell has not adopted these optional profile fields.

This change is one part of the Sandbox allocation work. It does not
enable Sandbox execution, change workspace setup, require a mutation webhook,
or close the deferred live Kata test.

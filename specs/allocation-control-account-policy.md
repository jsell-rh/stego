# Control account installation

The control account reuse audit found that fresh accounts could use retained
grants after control namespace replacement. The generated allocation policy
now reserves the allocator name and all declared control-worker account names.
This rule also applies to profiles without managed workload accounts.

Creation of these accounts requires `get` permission on the non-resource path
`/stego.dev/control-account-installation/<namespace>/<allocator>`. This is an
RBAC capability, not a network endpoint. The admission rule uses the Kubernetes
[authorizer library](https://kubernetes.io/docs/reference/using-api/cel/#kubernetes-authorizer-library).
The rule fails closed. It also checks overlapping `generateName` prefixes.
Unreserved names and ordinary account updates are not selected by this rule.

STEGO generates an unbound ClusterRole named
`<namespace>.<allocator>.control-account-installer`. An operator can bind this
role to a trusted deployment identity. The role supplies only the capability;
the deployment identity still needs its normal Kubernetes installation rights.
The allocator and application workers receive no installer capability. A grant
for one installation does not authorize another installation's account names.

Install the admission policy before account grants. Keep it installed while any
retained grant refers to its protected account names. Before a rename, account
removal, or policy removal, remove the old grants and verify cleanup. This rule
does not manage arbitrary external account grants or establish automatic safe
retirement of old declarations.

Restore control accounts only in a namespace controlled by trusted operators.
Remove untrusted Pod, account, token, and secret permissions before restoration.
The installer capability does not make a namespace with untrusted access safe.

The live regression keeps the old grants, replaces the control namespace, and
tests ordinary account creation, reserved names, generated names, and fresh
tokens. It then removes the former owner's fixture permissions and tests recovery
through a separate identity with the generated installer role. It checks that
neither allocator has this role and that the installer cannot use another
installation's capability. The live result is recorded below.

The first full CI run for this change failed in an endpoint test observer.
The observer required a variable list on every policy, but the new account
policy has no variables. Its type assertion panicked. The renderer accepts
the optional list. The test now accepts its absence, rejects other types, and
still requires the endpoint policy with the exact endpoint data. The test is
also part of focused account CI. The
[failed run record](allocation-control-account-renderer-failure.json) remains
available. No live check started with this failed full-suite prerequisite.

The regression passed at `931f712`. Both protected account names and their
overlapping generated names were denied to the replacement namespace owner.
An unreserved account name remained permitted. All five installer capability
checks passed, including denial to both allocators and denial across
installations. Both old tokens returned HTTP 401. The retained grants did not
change. After the former owner's fixture permissions were removed, the trusted
installer restored both accounts, and fresh tokens returned HTTP 200.

All 14 admission policies passed type checking. Independent cleanup found all
57 recorded resource paths absent, and the shared lease was released. Before
this run, seven focused generator checks, 37 runtime cases, 30 runner checks,
and all six full CI jobs passed. The full race step passed 34 packages.
The branch compiler artifact also passed source and two-build verification.
See the [qualification evidence](allocation-control-account-policy-evidence.json).

The first attempt on this source stopped during fixture installation because
a resource read failed. It did not reach permission tests. Its original status
and error text were not retained, so its cause is unknown. Independent cleanup
found all 32 created paths absent. API readiness and a read of the cleaned path
then passed. One fresh attempt with the same qualified source produced the
passing result above. The
[installation failure record](allocation-control-account-installation-failure.json)
remains available.

The qualified source is on STEGO main. Signed release publication and
Hypershell adoption remain pending. The account retirement and external grant
limits above still apply. This result does not close the enterprise goal.

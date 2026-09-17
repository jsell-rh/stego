# Allocation account checks

Source `2587af50528e18ad243fa9b33f22eaf92f8301bc` passed
[focused run 35223389996](https://github.com/jsell-rh/stego/actions/runs/35223389996),
job `105208834531`. Independent checks matched the exact source archive and
confirmed these six generated runtime tests:

- Fixed account-name vector and distinct owner and installation identities
- Quota before account and binding creation
- Rejection of changed namespace account annotations before writes
- Different account names after namespace reuse
- One owner selected during concurrent allocation
- No account or permission grant after quota failure

The generated runtime used a verified TLS API fixture. This fixture does not
run Kubernetes admission or RBAC. The namespace reuse result proves the
generated object names, not a live authorization decision.

Declaration validation and repeated generation also passed. The two rendered
installations each contain six admission policies with `failurePolicy: Fail`.
Independent inspection confirmed the namespace reservation, generated-name
guards, allocator capability, and installation-specific account-name checks.

| Record | SHA-256 |
| --- | --- |
| Source archive | `434df221fd3d1ed9a042114fe29eb52dc4daebdf8749575667b8cbb17c524f83` |
| Outer test log | `5fd4801fd4eaad8face25c12bfbdee3827f9ac0293cc94e55bb03fc91165375c` |
| Generated runtime log | `2e525af6e896088566679866312c0d73c3e7730f87d07b1b51d654cc34ecf4bd` |
| Primary manifest | `dda67b0c3f7c06fd3c8e50fb15906b0c051e1d50910e7f748afe094e49327c86` |
| Peer manifest | `866c12b46ace6c0fea89f098ee68bc987cd0c93867303a95baa7bed2de013051` |

The first workflow attempt, `35223217528` at `1eefeaf`, failed before a job
started. Its job-level environment used the unavailable `runner` context.
Commit `2628ab1` moved that setting into the running step. The later focused
checks passed. The failed attempt remains a failure.

The full suite passed at the earlier source `e1efece`, but that source predates
the namespace reservation and installation guards. It does not qualify the
latest source. The latest full suite and a real Kubernetes admission and
authorization gate remain required. Hypershell has not adopted this component
version. No compiler or Go test ran on the developer workstation.

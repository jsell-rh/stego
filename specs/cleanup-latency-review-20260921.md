# Gateway cleanup timing review

Two accepted workflows measured complete cleanup with 100 live accounts.
The observed upper bounds were 32.96 and 32.69 seconds. The 30-second target
remains open. These are two bounded workflow samples, not a production capacity
or latency distribution. See the [evidence](cleanup-latency-evidence-20260921.json).

| Completion observation | First workflow | Second workflow |
| --- | ---: | ---: |
| Gateway namespace absent | 13.62 s | 13.56 s |
| Sandbox namespace absent | 20.02 s | 21.32 s |
| SQL cleanup recorded | 21.17 s | 22.35 s |
| Console state namespace absent | 30.79 s | 30.78 s |
| All cleanup recorded | 31.83 s | 31.88 s |
| Complete proof, including account cleanup | 32.96 s | 32.69 s |

Times start at the observed HTTP 202 response. Each value is the first complete
observation after the last pending observation. It is not an exact transition
time. The complete result also checks that unrelated installation data remains.

The two workflows use identical namespace cleanup source. Hypershell first
waits for the Gateway namespace to disappear so that the Gateway stops creating
Sandbox work. It then waits for the Sandbox namespace. Retained state requires
both workload and SQL cleanup to be recorded. The two state namespaces are
already deleted together. The common allocator removes owned cluster bindings
before namespace deletion and reports completion only after resources are absent.

This order is application policy. STEGO supplies the bounded client, owned
resource deletion, pending result handling, observation commit, and controller
runtime. The application requests another check after one second while cleanup
is pending. The records do not support attributing the delay to a long retry
interval or to dependency conversion.

The last state namespace completed near 30.8 seconds in both samples. The
namespace observations show deletion requests and later absence. They do not
explain the delay inside Kubernetes. The retained allocator logs do not provide
a complete resource-specific history. Do not use these records as proof that
shorter Pod grace periods or fewer completion checks are safe.

The next timing observation must record the exact owned Pod UIDs, termination
states, namespace deletion requests, and finalizer conditions during the same
normal deletion. It must keep the current source, permissions, and cleanup
barriers. Use those results to select a change and its fault tests. Live Kata
and OpenShell Sandbox execution remain deferred; empty Sandbox namespace
cleanup does not establish the behavior of running Sandbox workloads.

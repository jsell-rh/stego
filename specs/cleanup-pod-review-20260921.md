# Pod observations during Gateway cleanup

Run `35549065540` completed normal Gateway cleanup with 100 live accounts in
an observed 32.34 seconds. The 30-second target remains open. See the
[retained observations](cleanup-pod-evidence-20260921.json).

The Gateway namespace contained both expected Pods in the read that ended at
01:08:42.217557 UTC. The read that ended at 01:08:48.516770 UTC contained no
Pods, with the same namespace UID verified after each read. Cleanup started
with the accepted response at 01:08:39.269681 UTC. Thus, the observed Pod absence
preceded the final cleanup observation by more than 20 seconds. This result
does not identify the delay in namespace removal or prove process termination.

The later reads retained the Sandbox and state namespaces. The two state
namespaces had API deletion timestamps of 01:09:02 UTC. They had no Pods in
these snapshots. One final namespace identity check was ambiguous because the
namespace disappeared between reads. The review does not treat that snapshot
as a verified namespace identity. The read that ended at 01:09:12.757688 UTC
contained none of the selected Gateway namespaces.

The observer made 853 bounded read requests during the workflow and retained
33 changed snapshots. These reads added diagnostic load. Transitions between
reads and clock differences remain possible. This is not a capacity result.

The raw observer calls `metadata.deletionTimestamp` `deletion_requested_at`.
The reviewed record uses `api_deletion_timestamp`. It does not change the value.
This API field is not an exact request time. Do not subtract the configured Pod
grace period to infer a request time. See the
[Kubernetes ObjectMeta reference](https://kubernetes.io/docs/reference/kubernetes-api/definitions/object-meta-v1-meta/).

Keep the existing cleanup order and absence checks. This evidence does not
justify shorter Pod grace periods, forced deletion, or removed finalizers.

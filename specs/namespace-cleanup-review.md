# Namespace cleanup review

Hypershell source `8a5e38d` completed the measured Gateway cleanup within
34.827 seconds. This exceeds the 30-second target. The
[source review](namespace-cleanup-review-evidence.json) records the cleanup
barriers and the limits of the observations.

STEGO checks ownership and object identity before deletion. It removes owned
cluster bindings first. Thus, a pending allocator result does not prove that
a namespace deletion request was accepted. Hypershell must continue to require
Gateway namespace absence before it removes the Sandbox namespace. Both state
namespaces require completed SQL and workload observations.

The [upstream Kubernetes v1.35.0 namespace controller](https://raw.githubusercontent.com/kubernetes/kubernetes/v1.35.0/pkg/controller/namespace/namespace_controller.go)
delays namespace events for five seconds. Its source explains that this lets
API servers observe deletion and storage replicas observe recent object
creation. The [resource deleter](https://raw.githubusercontent.com/kubernetes/kubernetes/v1.35.0/pkg/controller/namespace/deletion/namespaced_resources_deleter.go)
checks remaining resources before it removes its namespace finalizer.
The installed cluster reports v1.35.6 with a different source commit. This
review does not verify that exact implementation or assign each measured delay
to upstream behavior.

Keep the current dependency barriers, finalizers, and grace periods. The
application allocator already uses STEGO pending results. The workload adapter
can use the same API for its exact pending value. Provider failures, failed
state commits, and cancellation must remain errors. Candidate `2f3b4fe` adds
this adapter change and fault tests. Its hosted checks are pending. No new
cleanup time or capacity result is available.

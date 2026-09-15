# Deployment availability

Version 1.6.0 adds `DeploymentAvailable(object, owner, replicas)`. The helper
reads a Deployment returned by the generated Kubernetes client. It makes no
network requests and does not change the object.

The caller must supply its expected owner labels and a replica count from 1
through 10000. The object must have the Deployment API kind, a name, namespace,
UID, resource version, and matching owner labels. Desired objects and objects
with another owner cannot establish availability.

Availability requires the current generation, exactly the expected total,
updated, ready, and available replica counts, no unavailable replicas, and an
`Available=True` condition. A paused or deleting Deployment is not available.
A failed or unknown progress or replica-failure condition prevents success.
Missing status remains pending. Malformed counts or conditions return
`ErrResourceObservation`. Numbers retain their exact JSON integer value.

This checks Kubernetes availability. It does not prove application health,
certificate validity, route admission, or network reachability. Applications
must combine it with the checks required by their service contract. The helper
has no Hypershell names, field mappings, or phase policy.


Version 1.7.0 adds `PassthroughRouteAdmitted` and `PassthroughRouteTarget` for
OpenShift Routes. The target fixes the DNS host, Service, named target port, and
router. The check rejects foreign ownership, alternate backends, path routing,
wildcards, disabled backends, TLS termination at the router, and plaintext
access. Deletion or missing admission returns pending. Malformed status,
conflicting selected ingress entries, and duplicate conditions return an error.
Ingress and condition counts are bounded. The function makes no network calls.

The [OpenShift Route API](https://docs.redhat.com/en/documentation/openshift_container_platform/4.22/html/network_apis/route-route-openshift-io-v1)
has router admission conditions but no observed generation. A passing admission
check does not prove that the current backend serves traffic. The caller must
also verify TLS and the application through the selected hostname. It must
commit the observation against the application revision used for external work.
The helper contains no application entity names or hostname allocation policy.

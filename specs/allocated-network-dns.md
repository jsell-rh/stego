The user requires operator-approved Gateway destinations and support for
external PostgreSQL, including RDS provisioned outside Hypershell. The complete
network contract must account for external address changes. Common policy and
reconciliation code belongs in STEGO; Hypershell declares its required peers.

Standard Kubernetes NetworkPolicy selects addresses, Pods, and namespaces; it
does not provide service-name targets. See the
[Kubernetes API limits](https://kubernetes.io/docs/concepts/services-networking/network-policies/#what-you-cant-do-with-network-policies-at-least-not-yet).
OpenShift EgressFirewall supports DNS names, but its documentation identifies a
race between Pod and firewall DNS updates. The improved DNS resolver remains
Technology Preview in the 4.22 documentation. See
[OpenShift DNS behavior](https://docs.redhat.com/en/documentation/openshift_container_platform/4.22/html/network_security/egress-firewall).

Read-only checks on jshell on 2026-09-15 found `OVNKubernetes` and the namespaced
`egressfirewalls.k8s.ovn.org` CRD. This proves API availability only. No firewall
was created, and no DNS-change or traffic-enforcement result is claimed.
The saved cluster context reports OpenShift 4.22.11.

The user approved supported DNS-aware providers on 2026-09-15. Application
destination declarations must remain independent of the network provider.
STEGO must reject unsupported configurations and must not enable Technology
Preview features. No provider implementation has been enabled yet.

For example, the operator approves one PostgreSQL hostname on TCP port 5432.
An RDS failover can change the address behind that hostname. AWS recommends
connections through the DNS endpoint for this reason. See the
[RDS connection guidance](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_VPC.WorkingWithRDSInstanceinaVPC.html).
Application DNS resolution and network permission are separate operations.
The client can obtain a new IP address while an old policy still blocks it.

With standard NetworkPolicy only, STEGO would need a common mechanism to update
the approved IP rules. It must handle DNS cache lifetimes, failed lookups,
address removal, and restart. This work must not be left to each application.
With a DNS-aware network provider, STEGO can use that provider's mechanism,
but the deployment then requires a supported provider and verified behavior.
Neither choice removes the need for TLS identity checks and database credentials.

The proposed design keeps destination declarations independent of the network
provider. STEGO validates and generates the selected enforcement mechanism.
For in-cluster services, standard Pod and namespace selectors remain useful.
For external DNS destinations, a provider-specific implementation can be used
when its support and behavior meet the deployment requirements. The approved
design excludes OpenShift's Technology Preview resolver.
Provider selection still requires documented support and live traffic evidence.

The acceptance gate must retain these requirements under either choice:

1. Operator configuration defines the approved destinations and ports.
2. A DNS or policy error cannot enable general outbound access.
3. Verified TLS and service identity checks remain required.
4. Fresh connections test address changes, old-address removal, DNS failure,
   controller restart, regeneration, and denied unrelated destinations.
5. Tests retain separate Gateway, database, and Sandbox policy boundaries.

The public Gateway workflow has separate evidence for allocated namespace
network isolation. Those fixed Pod and IP rules do not prove external DNS
tracking, address replacement, or DNS failure behavior.

The [capability check](allocated-network-dns-capabilities-20260915.json) used
read-only cluster requests. The cluster uses the Default feature set, with
DNSNameResolver disabled. The EgressFirewall API is present; the Cilium policy
API is absent. API presence alone does not prove support or enforcement.
No provider or cluster feature was enabled by this check.

Provider acceptance must include the complete public Gateway workflow.
OpenShift documents router bypass and return-traffic limits for EgressFirewall.
The generated policy must preserve approved replies without permitting unrelated
outgoing connections. The EgressFirewall status schema has no observed-generation
field, so a status string alone cannot prove that a changed rule is enforced.
Keep live address replacement, removal, and denied connection checks in the gate.

A [source review](allocated-network-dns-source-review-20260915.json) of
OpenShift branch `release-4.22`, commit `e2082ef4`, found that the legacy DNS
path retains the previous IP list when a failed lookup returns no valid
answers. Its retry interval can increase to two minutes; that limit does not
expire the retained addresses. See the
[DNS cache update](https://github.com/openshift/ovn-kubernetes/blob/e2082ef4a1aaad8fa5acc7b24880394b60a4e8ae/go-controller/pkg/util/dns.go#L110)
and the
[address-set update](https://github.com/openshift/ovn-kubernetes/blob/e2082ef4a1aaad8fa5acc7b24880394b60a4e8ae/go-controller/pkg/ovn/dns_name_resolver/dns.go#L151).
This is source evidence, not a live DNS result. The installed image digest was
read, but its source commit remains unverified because registry access was
denied. No registry credential was read. Provider qualification must establish
what happens to old addresses after failed or empty DNS answers. A retry
interval alone is not an address-removal guarantee.

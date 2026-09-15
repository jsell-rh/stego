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

A question is pending: may STEGO use DNS-aware cluster controls, or must it use
standard Kubernetes NetworkPolicy only? The first choice can require a network
provider with supported DNS behavior. The second requires a separate mechanism
to update approved IP rules. A provider has not been selected or enabled.

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
when its support and behavior meet the deployment requirements. The user has
requested more detail about this choice; no decision has been recorded.
The proposal does not select OpenShift's Technology Preview resolver.

The acceptance gate must retain these requirements under either choice:

1. Operator configuration defines the approved destinations and ports.
2. A DNS or policy error cannot enable general outbound access.
3. Verified TLS and service identity checks remain required.
4. Fresh connections test address changes, old-address removal, DNS failure,
   controller restart, regeneration, and denied unrelated destinations.
5. Tests retain separate Gateway, database, and Sandbox policy boundaries.

The existing public Gateway workflow can continue while this choice is open.
Its passing result would not prove allocated namespace network isolation.

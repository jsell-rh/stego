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

A question is pending: may STEGO use DNS-aware cluster controls, or must it use
standard Kubernetes NetworkPolicy only? The first choice can require a network
provider with supported DNS behavior. The second requires a separate mechanism
to update approved IP rules. A provider has not been selected or enabled.

The acceptance gate must retain these requirements under either choice:

1. Operator configuration defines the approved destinations and ports.
2. A DNS or policy error cannot enable general outbound access.
3. Verified TLS and service identity checks remain required.
4. Fresh connections test address changes, old-address removal, DNS failure,
   controller restart, regeneration, and denied unrelated destinations.
5. Tests retain separate Gateway, database, and Sandbox policy boundaries.

The existing public Gateway workflow can continue while this choice is open.
Its passing result would not prove allocated namespace network isolation.

Automatic controller failover needs an explicit provider-write contract. This
document records an open design decision. It does not claim that STEGO supplies
distributed ownership or fencing today.

A lease can select a current controller. It cannot, by itself, make a provider
reject a request that an old controller already sent. API resource revisions
protect later status commits. They do not undo an external effect. Retained
cleanup records permit repair after a late effect, but that is a different
guarantee from preventing the effect.

The user's pending choice is between these guarantees:

| Policy | Required behavior | Consequence |
| --- | --- | --- |
| Require rejection of stale writes | The provider must check the controller's authority at the write boundary. Automatic takeover must fail closed if this cannot be proved. | Some providers need an extension or operator-controlled recovery. |
| Permit later repair | Takeover can proceed while an old external effect remains possible. Retained intent and repair must survive restart. | Temporary stale effects remain possible and must be part of the public contract. |

The recommended policy is to require stale-write rejection for automatic
failover. An operator action alone is not proof that an old request cannot
complete. Recovery instructions must identify the evidence that makes a new
writer safe. No failover implementation should begin from a replica count or
lease-expiry check alone.

The checked Keycloak test image reports version 26.7.3. In that release, the
client update endpoint accepts a `ClientRepresentation`, checks permissions and
client policies, and updates the model. Its update path does not check an
`If-Match` header. The delete path also uses permission and policy checks.
See the [Keycloak 26.7.3 client resource](https://github.com/keycloak/keycloak/blob/26.7.3/services/src/main/java/org/keycloak/services/resources/admin/ClientResource.java).
This source review does not prove an atomic stale-writer rejection contract for
the standard endpoint. A provider extension would need separate runtime tests.

Before a provider is qualified for automatic failover, tests must hold an old
request in flight, transfer authority, and then release the old request. Cover
create, update, and delete separately, including deletion and recreation of an
external object with the same name. The provider must reject an obsolete write
at its commit boundary. A failed later status update is insufficient evidence.

STEGO should own the authority contract, bounded execution, recovery state, and
required capability checks. The provider adapter should supply the actual write
preconditions and their evidence. Hypershell should supply resource meaning,
access rules, and provider selection. No Hypershell-specific lease mechanism is
needed in STEGO.

The current watch-reconnect retry fix is independent of this decision. It keeps
one process's bounded queue across transport sessions. It supplies neither a
distributed lease nor provider fencing. Retry persistence after controller
process restart also remains open.

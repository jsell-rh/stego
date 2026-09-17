# Close clients found through inventory

An application can declare current and legacy ownership at the same time.
Inventory can find a current client before an account journal exists.
`PrepareCloseExisting` saves the provider ID and irreversible closure intent.
It makes no provider request. Before this change, it selected legacy ownership
when legacy keys were configured. Later cleanup rejected the current client.

Provider 0.18.1 can recover that closed journal. If deletion rejects legacy
ownership, it checks current ownership at the saved provider ID. It rejects
mixed ownership and undeclared reserved keys. It saves the current binding with
closure still set, then checks ownership again through `DeleteClient`. A failed
save prevents deletion. A later retry can recover a lost save acknowledgement.
It does not search by the public client name or enable access.

Application policy still supplies the client name, expected ownership values,
and authorization to close the resource. The provider does not copy expected
ownership from the remote representation. This change adds no Hypershell names
or application rules to STEGO.

Regression tests cover direct and prepared closure, restart, repeated deletion,
late clients, rejected and unconfirmed saves, changed identity, mixed ownership,
and ownership changes between requests. The real Keycloak test covers current
and legacy clients without journals and confirms that mixed ownership remains
unchanged. CI results and application adoption are pending.

Provider reads and remote writes are separate operations. The existing writer
gate applies, but it cannot make an external administrator's changes atomic
with the provider request. This change does not claim such a guarantee.

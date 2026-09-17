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
unchanged. Compiler results are recorded below. Application checks remain in progress.

Provider reads and remote writes are separate operations. The existing writer
gate applies, but it cannot make an external administrator's changes atomic
with the provider request. This change does not claim such a guarantee.

## Verified branch result

[Compiler run 35211690639](https://github.com/jsell-rh/stego/actions/runs/35211690639)
passed all six jobs for `e206b41`. The full race suite passed in 34 packages.
The three new closure tests passed with and without telemetry. The real
Keycloak test passed in 63.10 seconds, including current and legacy clients
without journals and unchanged mixed ownership after rejection. Provider
container cleanup passed. The retained provider log has SHA-256
`76fac83c4277d0013b6299f3672fc96f9aa3a3a65f49651449d63be838b93681`.

[Artifact run 35211690739](https://github.com/jsell-rh/stego/actions/runs/35211690739)
produced matching compiler bytes from two separate source trees and caches.
Independent inspection matched all 1,199 source files, binary build identity,
and checksums. The compiler SHA-256 is
`93a74182dd6b90e55d703ab1f472fbba180a253177003cdee0cf7b830030b6d4`.
This branch artifact is unsigned. Main checks, a signed package, and Hypershell
adoption remain pending. No compiler or Go test ran on the workstation.


## Main and immutable release

[Main run 35212383523](https://github.com/jsell-rh/stego/actions/runs/35212383523)
passed all six jobs for the same source `e206b41`. Independent inspection again
confirmed the new tests in both telemetry variants, the real Keycloak result,
and 34 passing packages. The main compiler log SHA-256 is
`61642f5d30f18f54a0714595530f9ece20cde1033422f113f0b9cf82baec72c1`.

[Signed artifact run 35212383507](https://github.com/jsell-rh/stego/actions/runs/35212383507)
passed. Local signature verification matched the exact source, main workflow,
compiler bytes, and build record. The four rejection checks also passed.

The [immutable compiler release](https://github.com/jsell-rh/stego/releases/tag/compiler-e206b41cb079280d9ed467f3cd3e7bd5d6037cdc)
has ID `390633473`. All four draft assets were downloaded and matched against
the independently authenticated package before publication. The published tag
names the exact commit, and release immutability is enabled. The common
installer then accepted the published package with the same verification record.
No compiler or Go test was executed on the workstation.

Hypershell has imported generated output from this release on its work branch.
Hosted regeneration passed all three modules. The real unknown-client workflow
and complete application checks remain in progress. The compiler result alone
does not close those application requirements.

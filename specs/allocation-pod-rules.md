# Application Pod rules

This extension is under development. It is not a qualified compiler release.
Hypershell has not adopted it.

An allocation profile can add `pod_validations`. Each entry has a CEL
`expression` and a fixed `message`. A profile must already require a Pod runtime
or allocated account. Rules add to the common admission checks. They cannot
change the policy selector, common checks, variables, failure mode, or binding.
The worker receives no expression code or policy-write permission.

The compiler permits 1 to 16 rules, at most 4096 bytes per expression, and at
most 256 bytes per message. It rejects duplicate expressions, invalid CEL,
non-boolean results, template delimiters, and control characters in messages.
Parser depth, recovery attempts, and expression nodes have bounds. The selected
CEL implementation is [CEL-Go 0.32.0](https://github.com/cel-expr/cel-go/releases/tag/v0.32.0).
Only standard CEL functions are available. This extension does not enable
experimental Kubernetes APIs or CEL libraries.

Rules can use `object`, `oldObject`, `namespaceObject`, and `request`.
`variables.containers` combines normal and init containers. No other policy
variable is exposed by the compiler. Rules must account for absent fields.
A policy evaluation error rejects the request.

The compiler checks syntax and result type. Kubernetes must also check the
rules against its installed Pod schema and cost limits before the installer
grants workload access. Dynamic field access in the compiler is not proof of
that cluster check. Application tests must cover their allowed and denied
requests. The compiler does not infer application intent from a rule that
returns true.

Image selection, credential mounts, and domain storage ownership can use these
additional rules. Keep those decisions in the application declaration. STEGO
continues to provide namespace ownership, runtime and account restrictions,
and the policy installation shape.

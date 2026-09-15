# HTTP route composition

HTTP generators declare their route patterns and their public or protected
group before rendering. The compiler validates each component context first,
then checks all declared patterns on separate Go HTTP multiplexers. It also
checks the public multiplexer's fallback to the protected handler. No handler
code runs, and no network connection is opened during this check.

This catches conflicts that one component cannot detect alone. For example,
`base_path: /livez` is a valid literal prefix. However, in `rest-crud`, it gives
the metadata handler and health monitor the same `GET /livez` registration.
The same conflict exists at `/readyz`. Both declarations passed the earlier
validation gate. The new regression reproduced that acceptance before the fix.

`validate`, `plan`, and `apply` now reject these conflicts before any generator
runs. A generator's route declaration is copied before rendering. Its emitted
route patterns must match that copy. Changing the returned declaration during
generation cannot bypass the check. Direct assembler calls also validate the
actual route expressions before producing shared files.

Wiring routes must call `mux.Handle` or `mux.HandleFunc` for protected routes,
or the corresponding `topMux` method for public routes. The pattern must be a
string literal. The compiler reads the Go syntax tree; it does not run the
handler expression. Supported generated code is unchanged. REST, health,
application HTTP, and browser backend generators provide the declarations.

Checks cover public probe conflicts, protected duplicates, wildcard conflicts,
the public fallback, invalid registrations, legacy mux mode, declaration changes,
validation before rendering, and CLI file preservation. A protected application
mount at `/` can still coexist with public probes and root metadata.
[Full compiler CI](https://github.com/jsell-rh/stego/actions/runs/34959118556)
passed at `1edd407`, including the race suite and SQL provisioning checks.

This validates registration on the composed multiplexers. It does not inspect
routes inside an application-supplied handler or establish the access policy
for overlapping public and protected patterns. Those behaviors still require
their component and application checks. The broader compiler and application
acceptance requirements remain open.

# Review: Task 038 — Generated Runtime Configuration (PORT Environment Variable)

## Summary

The core change is correct: generated `main.go` now reads `PORT` from `os.Getenv("PORT")` with `"8080"` fallback, exactly matching the spec's required pattern. The `Port` field was cleanly removed from `AssemblerInput`, the reconciler no longer resolves port from component config, and all tests pass. Two inconsistencies remain from the `fmt` removal.

## Findings

- [ ] **`stdlibAliases()` `covered` map still includes `"fmt"`** (`assembler.go:1617`): This change correctly removed `"fmt"` from the conditional alias list (it was previously added when `hasRoutes`, used by `fmt.Sprintf` in `writeServerStart`). However, the `covered` map on line 1617 still contains `"fmt": true`. The `covered` map's purpose is to prevent double-adding aliases that are already handled by the conditional checks above — but `"fmt"` is no longer handled by any conditional check. This means if a component declares `StdlibImports: ["fmt"]`, the import will be added to `stdlibNeeded` (and emitted in the import block), but `stdlibAliases` won't return `"fmt"` in its list because `covered` blocks the fallback loop. The alias won't be reserved in disambiguation maps, so a non-stdlib import with base name `"fmt"` could shadow the stdlib import, causing a compile error in the generated code. Fix: remove `"fmt"` from the `covered` map.

- [ ] **Stale comment in `constructorRename.PreReserved`** (`assembler.go:578`): The comment lists `fmt` as a stdlib import alias that triggers `PreReserved=true`: `"stdlib import alias (log, fmt, http, os, sql)"`. Since `fmt` was removed from `assemblerInternalVars` (it is no longer a reserved variable or stdlib alias in the generated code), the comment should be updated to remove `fmt` from this list.

// Package compiler assembles component wirings, slot bindings, and fill
// declarations into shared generated files (cmd/main.go, go.mod).
package compiler

import (
	"bytes"
	"fmt"
	"go/format"
	"path"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// AssemblerInput carries everything needed to assemble shared files.
type AssemblerInput struct {
	// ModuleName is the Go module path (e.g. "github.com/myorg/user-service").
	ModuleName string

	// ServiceName is the service name from the service declaration.
	ServiceName string

	// GoVersion is the Go version for go.mod (e.g. "1.22").
	GoVersion string

	// Port is the HTTP listen port.
	Port int

	// Wirings from component generators, in dependency order.
	Wirings []ComponentWiring

	// SlotBindings from the service declaration.
	SlotBindings []types.SlotDeclaration

	// SlotsPackage is the relative import path (under ModuleName) for the
	// generated slot operators package. If empty, slot wiring is omitted.
	SlotsPackage string

	// OutDirName is the name of the output directory relative to the project
	// root (e.g. "out"). With go.mod at the project root, import paths for
	// generated packages must include this prefix so they resolve correctly
	// (e.g. "<module>/out/internal/api" instead of "<module>/internal/api").
	// Fill imports do NOT include this prefix since fills live at the project
	// root level (e.g. "<module>/fills/<name>").
	OutDirName string
}

// ComponentWiring pairs a component name with its generator's wiring output.
type ComponentWiring struct {
	Name   string
	Wiring *gen.Wiring
}

// Assemble produces the shared generated files from component wirings and
// slot bindings. Currently produces cmd/main.go and go.mod. No files are
// ever generated under fills/ — that directory is human-owned.
func Assemble(input AssemblerInput) ([]gen.File, error) {
	if input.ModuleName == "" {
		return nil, fmt.Errorf("ModuleName must not be empty")
	}
	if input.GoVersion == "" {
		return nil, fmt.Errorf("GoVersion must not be empty")
	}
	if input.Port <= 0 {
		input.Port = 8080
	}

	mainGo, err := generateMainGo(input)
	if err != nil {
		return nil, fmt.Errorf("generating main.go: %w", err)
	}

	goMod := generateGoMod(input)

	files := []gen.File{mainGo, goMod}

	// Invariant: never generate under fills/.
	for _, f := range files {
		if strings.HasPrefix(f.Path, "fills/") {
			return nil, fmt.Errorf("assembler bug: generated file %q under fills/ directory", f.Path)
		}
	}

	return files, nil
}

func generateGoMod(input AssemblerInput) gen.File {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "module %s\n\n", input.ModuleName)
	fmt.Fprintf(&buf, "go %s\n", input.GoVersion)

	// go.mod is placed at the project root (not inside out/) so that both
	// generated packages (under out/) and fill packages (under fills/) are
	// within the module root. This makes all import paths resolvable as
	// intra-module packages without requiring replace directives.

	return gen.File{
		Path:    "go.mod",
		Content: buf.Bytes(),
	}
}

func generateMainGo(input AssemblerInput) (gen.File, error) {
	var buf bytes.Buffer

	buf.WriteString("package main\n\n")

	hasRoutes := hasAnyRoutes(input)
	hasSlots := len(input.SlotBindings) > 0 && input.SlotsPackage != ""

	// Validate no duplicate (slot, entity, operator) triples before processing.
	if err := validateSlotBindingUniqueness(input.SlotBindings); err != nil {
		return gen.File{}, err
	}

	// Validate no derived variable name collisions (distinct raw composite
	// keys that normalize to the same camelCase identifier, e.g.
	// "before_create" and "before__create" both produce "beforeCreate").
	if err := validateSlotVarNameUniqueness(input.SlotBindings); err != nil {
		return gen.File{}, err
	}

	// Validate no intra-wiring constructor base name collisions.
	if err := validateConstructorUniqueness(input.Wirings); err != nil {
		return gen.File{}, err
	}

	// Validate middleware wiring: if MiddlewareConstructor is set,
	// MiddlewareWrapExpr must also be set so the assembler knows how
	// to invoke the middleware (per checklist item 113).
	for _, cw := range input.Wirings {
		if cw.Wiring != nil && cw.Wiring.MiddlewareConstructor != nil && cw.Wiring.MiddlewareWrapExpr == "" {
			return gen.File{}, fmt.Errorf("component %q declares MiddlewareConstructor but no MiddlewareWrapExpr — generators must specify how the middleware wraps the handler (e.g. \"%%s(%%s)\" for function-type middleware)", cw.Name)
		}
	}

	// Compute which constructor entries are consumed (transitively reachable
	// from route references or middleware wrapping). Constructors without any
	// downstream consumer would produce "declared and not used" compile errors.
	// Effective hasDB is true only if at least one consumed constructor needs DB.
	consumed, effectiveHasDB := computeConsumedConstructors(input, hasRoutes)
	hasDB := effectiveHasDB

	// Build slot operator variable names by entity so we can inject them
	// into handler constructor calls.
	slotVarsByEntity := buildSlotVarsByEntity(input.SlotBindings, hasSlots)
	// Collect ALL slot var names (including entity-less ones) so constructor
	// disambiguation can avoid collisions with slot operators.
	allSlotVarNames := collectAllSlotVarNames(input.SlotBindings, hasSlots)

	// Compute which wirings have at least one consumed constructor so
	// that writeMainImports only emits imports for packages that are
	// actually referenced. An unconsumed wiring's imports would be
	// unused — a Go compile error (finding 30).
	consumedWirings := make(map[int]bool)
	for key := range consumed {
		consumedWirings[key.WiringIndex] = true
	}

	imports := writeMainImports(&buf, input, hasRoutes, hasDB, hasSlots, consumedWirings)

	buf.WriteString("func main() {\n")

	if hasDB {
		writeDBSetup(&buf)
	}

	// Slot wiring — create operators before constructors so they can be
	// injected as handler constructor arguments.
	if hasSlots {
		writeSlotWiring(&buf, input, slotVarsByEntity, hasDB, consumedWirings)
	}

	wiringRenames, err := writeConstructors(&buf, input, slotVarsByEntity, allSlotVarNames, hasDB, hasRoutes, imports, consumed)
	if err != nil {
		return gen.File{}, err
	}

	if hasRoutes {
		writeRouteRegistration(&buf, input, wiringRenames)
		writeServerStart(&buf, input, wiringRenames)
	}

	buf.WriteString("}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting main.go: %w (raw:\n%s)", err, buf.String())
	}

	return gen.File{
		Path:    "cmd/main.go",
		Content: formatted,
	}, nil
}

// importResult bundles the outputs of writeMainImports: per-wiring import alias
// renames and the complete set of non-stdlib import aliases assigned. The alias
// set is used to seed constructor variable disambiguation maps so that
// constructor variables cannot shadow component, fill, or slots import aliases.
type importResult struct {
	// Renames maps wiring index → (original base name → disambiguated alias)
	// for cases where a component's import alias was disambiguated.
	Renames map[int]map[string]string

	// NonStdlibAliases is the set of all non-stdlib import aliases assigned
	// during import block construction (component aliases, fill aliases, and
	// the slots alias when present). Constructor variable disambiguation must
	// reserve all of these to prevent shadowing.
	NonStdlibAliases map[string]bool
}

// writeMainImports writes the import block and returns per-wiring import alias
// renames plus the complete set of non-stdlib import aliases. The renames allow
// constructor and route expressions to be updated when their component's import
// alias is disambiguated. The alias set allows constructor variable
// disambiguation to avoid shadowing import aliases.
func writeMainImports(buf *bytes.Buffer, input AssemblerInput, hasRoutes, hasDB, hasSlots bool, consumedWirings map[int]bool) importResult {
	buf.WriteString("import (\n")

	// Standard library imports.
	var stdImports []string
	if hasDB {
		stdImports = append(stdImports, `"database/sql"`)
	}
	// fmt is only used in writeServerStart which is gated on hasRoutes.
	if hasRoutes {
		stdImports = append(stdImports, `"fmt"`)
	}
	// log is used in writeDBSetup and writeServerStart.
	if hasDB || hasRoutes {
		stdImports = append(stdImports, `"log"`)
	}
	if hasRoutes {
		stdImports = append(stdImports, `"net/http"`)
	}
	if hasDB {
		stdImports = append(stdImports, `"os"`)
	}
	for _, imp := range stdImports {
		fmt.Fprintf(buf, "\t%s\n", imp)
	}

	// All non-stdlib imports share a SINGLE disambiguation namespace so that
	// component, fill, and slots aliases cannot collide with each other.
	var compImports []string
	seen := make(map[string]bool)      // full import path → already added
	aliases := make(map[string]int)    // base alias → count (for disambiguation)
	aliasUsed := make(map[string]bool) // tracks the exact alias string used

	// Seed the disambiguation maps with stdlib import aliases so that
	// non-stdlib imports cannot shadow them. A component import like
	// "internal/sql" would otherwise get alias "sql", shadowing the
	// stdlib "database/sql" import.
	for _, name := range stdlibAliases(hasRoutes, hasDB) {
		aliases[name]++
		aliasUsed[name] = true
	}

	// Track per-wiring import alias renames for propagation to constructor
	// and route expressions.
	importRenames := make(map[int]map[string]string)

	// Track ALL non-stdlib import aliases for constructor var disambiguation.
	nonStdlibAliases := make(map[string]bool)

	// Track per-path alias assignments so that when multiple wirings declare
	// the same import path, the rename mapping from the representative entry
	// is propagated to all duplicate entries (finding 22).
	pathRenames := make(map[string]map[string]string) // fullPath → (base → alias) rename, nil if no rename needed

	// Slots package import — registered first so its hardcoded alias "slots"
	// is reserved before any dynamic disambiguation runs.
	if hasSlots {
		fullPath := generatedImportPath(input.ModuleName, input.OutDirName, input.SlotsPackage)
		if !seen[fullPath] {
			seen[fullPath] = true
			slotsAlias := disambiguateAlias("slots", aliases, aliasUsed)
			nonStdlibAliases[slotsAlias] = true
			compImports = append(compImports, fmt.Sprintf("\t%s %q", slotsAlias, fullPath))
		}
	}

	// Component imports — only for wirings with at least one consumed
	// constructor. Unconsumed constructors are not emitted by
	// writeConstructors, so their package imports would be unused — a Go
	// compile error (finding 30, checklist item 124).
	for i, cw := range input.Wirings {
		if cw.Wiring == nil {
			continue
		}
		if !consumedWirings[i] {
			continue
		}
		for _, imp := range cw.Wiring.Imports {
			fullPath := generatedImportPath(input.ModuleName, input.OutDirName, imp)
			base := path.Base(imp)
			if seen[fullPath] {
				// Propagate rename from the representative entry so this
				// wiring's constructor and route expressions are updated to
				// use the disambiguated alias.
				if renames, ok := pathRenames[fullPath]; ok && renames != nil {
					if importRenames[i] == nil {
						importRenames[i] = make(map[string]string)
					}
					for oldBase, newAlias := range renames {
						importRenames[i][oldBase] = newAlias
					}
				}
				continue
			}
			seen[fullPath] = true
			alias := disambiguateAlias(base, aliases, aliasUsed)
			nonStdlibAliases[alias] = true
			compImports = append(compImports, fmt.Sprintf("\t%s %q", alias, fullPath))
			if alias != base {
				if importRenames[i] == nil {
					importRenames[i] = make(map[string]string)
				}
				importRenames[i][base] = alias
				pathRenames[fullPath] = map[string]string{base: alias}
			} else {
				pathRenames[fullPath] = nil
			}
		}
	}

	// Fill imports — deduplicated across all slot bindings.
	// Gated on hasSlots: fill aliases are only referenced by writeSlotWiring,
	// which is also gated on hasSlots. When SlotsPackage is empty, slot wiring
	// is omitted entirely, so fill imports would be unused — a Go compile
	// error (finding 31, checklist item 120).
	if hasSlots {
		fillNames := collectFillNames(input.SlotBindings)
		for _, name := range fillNames {
			baseAlias := rawFillImportAlias(name)
			alias := disambiguateAlias(baseAlias, aliases, aliasUsed)
			nonStdlibAliases[alias] = true
			fullPath := input.ModuleName + "/fills/" + name
			compImports = append(compImports, fmt.Sprintf("\t%s %q", alias, fullPath))
		}
	}

	if len(compImports) > 0 {
		buf.WriteString("\n")
		for _, imp := range compImports {
			fmt.Fprintln(buf, imp)
		}
	}

	buf.WriteString(")\n\n")

	return importResult{
		Renames:          importRenames,
		NonStdlibAliases: nonStdlibAliases,
	}
}

func writeDBSetup(buf *bytes.Buffer) {
	buf.WriteString("\tdsn := os.Getenv(\"DATABASE_URL\")\n")
	buf.WriteString("\tif dsn == \"\" {\n")
	buf.WriteString("\t\tlog.Fatal(\"DATABASE_URL environment variable is required\")\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tdb, err := sql.Open(\"postgres\", dsn)\n")
	buf.WriteString("\tif err != nil {\n")
	buf.WriteString("\t\tlog.Fatal(err)\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tdefer db.Close()\n\n")
}

// collectAllSlotVarNames returns the set of all slot operator variable names
// that will be emitted by writeSlotWiring. This is used to seed the constructor
// variable disambiguation maps so that constructor vars cannot collide with
// slot operator vars in the same function scope.
func collectAllSlotVarNames(bindings []types.SlotDeclaration, hasSlots bool) map[string]bool {
	result := make(map[string]bool)
	if !hasSlots {
		return result
	}
	for _, sb := range bindings {
		if len(sb.Gate) > 0 {
			result[slotVarName(sb.Slot, sb.Entity, "Gate")] = true
		}
		if len(sb.Chain) > 0 {
			result[slotVarName(sb.Slot, sb.Entity, "Chain")] = true
		}
		if len(sb.FanOut) > 0 {
			result[slotVarName(sb.Slot, sb.Entity, "FanOut")] = true
		}
	}
	return result
}

// assemblerInternalVars returns the set of variable names that assembler-
// internal emitter functions (writeDBSetup, writeRouteRegistration,
// writeServerStart) introduce into the main() function scope, PLUS standard
// library import aliases that are used after constructor declarations (and
// would be shadowed by a local variable of the same name). Constructor
// variable disambiguation must reserve all of these to prevent collisions.
func assemblerInternalVars(hasDB, hasRoutes bool) map[string]bool {
	vars := make(map[string]bool)
	if hasDB {
		vars["dsn"] = true
		vars["db"] = true
		vars["err"] = true
		// stdlib import aliases used in writeDBSetup (before constructors,
		// but reserve defensively to prevent shadowing for any future post-
		// constructor usage).
		vars["sql"] = true
		vars["os"] = true
	}
	if hasRoutes {
		vars["mux"] = true
		vars["addr"] = true
		// stdlib import aliases used in writeRouteRegistration and
		// writeServerStart, which run AFTER constructors. A constructor
		// variable with the same name (e.g. logger.NewLog() → "log")
		// would shadow the import, breaking post-constructor references.
		vars["fmt"] = true
		vars["http"] = true
	}
	if hasDB || hasRoutes {
		// "log" is used in writeServerStart (after constructors) when
		// hasRoutes, and in writeDBSetup (before constructors) when hasDB.
		// Reserve whenever it is imported.
		vars["log"] = true
	}
	return vars
}

// constructorRename tracks a single constructor variable's original and
// disambiguated names, along with the constructor index within its wiring.
type constructorRename struct {
	ConstructorIndex int
	OriginalVar      string
	FinalVar         string
	// PreReserved is true when the rename was caused by collision with an
	// assembler-internal variable (mux, addr, db, dsn, err) or stdlib
	// import alias (log, fmt, http, os, sql). Pre-reserved renames must
	// NOT be applied to route expressions because the route's reference
	// to the identifier (e.g. mux.HandleFunc) refers to the assembler's
	// own variable, not the constructor. The constructor was directly
	// emitted as FinalVar; no route was ever written referencing the
	// constructor by OriginalVar.
	PreReserved bool
}

// writeConstructors emits constructor variable declarations and returns
// per-wiring rename lists. Each entry maps a wiring index to the list of
// constructors that were renamed (original → final variable name), keyed by
// constructor index within the wiring. Using a per-constructor-index structure
// (instead of map[string]string keyed by base name) avoids overwrite when two
// constructors in the same wiring derive the same base variable name.
// imports carries per-wiring import alias renames and the complete set of
// constructorKey uniquely identifies a constructor within the assembler input
// by its wiring index and constructor index within that wiring.
type constructorKey struct {
	WiringIndex      int
	ConstructorIndex int
}

// computeConsumedConstructors determines which constructors are transitively
// reachable from downstream consumers (route expressions, middleware wrapping).
// A constructor is consumed if:
//  1. Its raw variable name appears in a route expression (routes reference
//     constructor variables like "userHandler.Create"), OR
//  2. It is the middleware constructor (used by writeServerStart), OR
//  3. It is a dependency of another consumed constructor (via ConstructorDeps
//     or by referencing another constructor's variable in its expression).
//
// Constructors not in the returned set have no consumer in the generated code
// and must not be emitted (Go rejects unused local variables).
//
// Also returns effectiveHasDB: true only if at least one consumed constructor
// declares NeedsDB. This prevents emitting writeDBSetup when the only
// NeedsDB constructor is itself unconsumed.
func computeConsumedConstructors(input AssemblerInput, hasRoutes bool) (map[constructorKey]bool, bool) {
	// Build all constructor entries with their base var names.
	type entry struct {
		key     constructorKey
		baseVar string
		deps    []string
		expr    string
		needsDB bool
	}
	var entries []entry
	varToEntries := make(map[string][]int) // baseVar → entry indices

	for i, cw := range input.Wirings {
		if cw.Wiring == nil {
			continue
		}
		for j, constructor := range cw.Wiring.Constructors {
			baseVar := rawConstructorVarName(constructor)
			var deps []string
			if cw.Wiring.ConstructorDeps != nil {
				deps = cw.Wiring.ConstructorDeps[j]
			}
			idx := len(entries)
			entries = append(entries, entry{
				key:     constructorKey{WiringIndex: i, ConstructorIndex: j},
				baseVar: baseVar,
				deps:    deps,
				expr:    constructor,
				needsDB: cw.Wiring.NeedsDB,
			})
			varToEntries[baseVar] = append(varToEntries[baseVar], idx)
		}
	}

	consumed := make(map[constructorKey]bool)

	if !hasRoutes {
		// No routes means no route registration and no server start.
		// No constructor variables are referenced by any generated code.
		return consumed, false
	}

	// Step 1: Mark constructors directly consumed by routes.
	// Route expressions reference constructor variables by their raw name
	// (e.g. "userHandler.Create" references the "userHandler" variable).
	// Routes are generated by a specific wiring's generator and reference
	// that wiring's constructors — so only same-wiring constructors are
	// marked. When multiple wirings produce constructors with the same
	// baseVar, a route from wiring[i] referencing "store" means wiring[i]'s
	// constructor, not another wiring's. After disambiguation, routes are
	// updated via applyConstructorRenames to reference the disambiguated
	// name — see checklist item 123.
	for i, cw := range input.Wirings {
		if cw.Wiring == nil {
			continue
		}
		for _, route := range cw.Wiring.Routes {
			for baseVar, indices := range varToEntries {
				if containsIdentRef(route, baseVar) {
					for _, idx := range indices {
						// Only mark constructors from the same wiring as the route.
						if entries[idx].key.WiringIndex == i {
							consumed[entries[idx].key] = true
						}
					}
				}
			}
		}
	}

	// Step 2: Mark the middleware constructor as consumed (used by writeServerStart).
	for i, cw := range input.Wirings {
		if cw.Wiring == nil || cw.Wiring.MiddlewareConstructor == nil {
			continue
		}
		idx := *cw.Wiring.MiddlewareConstructor
		if idx >= 0 && idx < len(cw.Wiring.Constructors) {
			consumed[constructorKey{WiringIndex: i, ConstructorIndex: idx}] = true
		}
	}

	// Step 3: Transitively mark dependencies of consumed constructors.
	// Uses both ConstructorDeps (structured metadata) and expression-based
	// variable reference detection as a fallback.
	//
	// When multiple entries share the same baseVar (e.g. two wirings both
	// produce "store"), only the FIRST (canonical) entry retains the
	// original variable name after disambiguation — subsequent entries
	// get numeric suffixes ("store2", "store3"). Dependency expressions
	// reference the original name, so they depend on the canonical entry.
	// Marking all entries with the same baseVar would cause non-canonical
	// entries (which are renamed and never referenced) to be emitted as
	// unused variables — a Go compile error. See checklist item 123.
	//
	// Iterate until no new entries are added (fixed-point).
	changed := true
	for changed {
		changed = false
		for _, e := range entries {
			if !consumed[e.key] {
				continue
			}
			// Mark explicit deps from ConstructorDeps.
			for _, dep := range e.deps {
				if depIndices, ok := varToEntries[dep]; ok {
					// Only mark the canonical (first) entry for this baseVar.
					// After disambiguation, only the first entry retains the
					// original variable name that the expression references.
					depKey := entries[depIndices[0]].key
					if !consumed[depKey] {
						consumed[depKey] = true
						changed = true
					}
				}
			}
			// Also check if the constructor expression references other
			// constructor variables by name (fallback for when ConstructorDeps
			// is not set). Uses word-boundary-aware matching to prevent
			// "store" from matching within "datastore" — see checklist item 122.
			if argStart := strings.Index(e.expr, "("); argStart >= 0 {
				args := e.expr[argStart:]
				for depVar, depIndices := range varToEntries {
					if depVar == e.baseVar {
						continue // skip self-reference
					}
					if containsBareIdent(args, depVar) {
						// Only the canonical (first) entry retains the name.
						depKey := entries[depIndices[0]].key
						if !consumed[depKey] {
							consumed[depKey] = true
							changed = true
						}
					}
				}
			}
		}
	}

	// Compute effective hasDB: true only if at least one consumed constructor
	// has NeedsDB set.
	effectiveHasDB := false
	for _, e := range entries {
		if consumed[e.key] && e.needsDB {
			effectiveHasDB = true
			break
		}
	}

	return consumed, effectiveHasDB
}

// containsIdentRef checks whether s contains a reference to the identifier
// name followed by a dot, at a word boundary. This matches patterns like
// "userHandler.Create" for identifier "userHandler".
func containsIdentRef(s, name string) bool {
	target := name + "."
	i := 0
	for i < len(s) {
		idx := strings.Index(s[i:], target)
		if idx < 0 {
			return false
		}
		absIdx := i + idx
		if absIdx == 0 || !isIdentChar(s[absIdx-1]) {
			return true
		}
		i = absIdx + len(target)
	}
	return false
}

// non-stdlib import aliases from writeMainImports; constructor expressions are
// updated to reference the disambiguated aliases, and constructor variable
// names are disambiguated against all import aliases to prevent shadowing.
// constructorEntry holds metadata for a single constructor during topological
// sorting. It preserves the wiring and constructor indices needed for rename
// tracking and slot injection after sorting.
type constructorEntry struct {
	WiringIndex      int
	ConstructorIndex int
	RawExpr          string   // original constructor expression
	BaseVar          string   // derived variable name before disambiguation
	Deps             []string // variable names this constructor depends on
}

func writeConstructors(buf *bytes.Buffer, input AssemblerInput, slotVarsByEntity map[string][]string, slotVarNames map[string]bool, hasDB, hasRoutes bool, imports importResult, consumed map[constructorKey]bool) (map[int][]constructorRename, error) {
	varNames := make(map[string]int) // for collision detection
	varUsed := make(map[string]bool)
	wiringRenames := make(map[int][]constructorRename)

	// Track assembler-internal identifiers that the assembler itself emits
	// into the function body (mux, addr, db, dsn, err) and stdlib import
	// aliases (log, fmt, http, os, sql). Renames caused by collision with
	// these identifiers must NOT be applied to route expressions because
	// the route's reference to the identifier (e.g. mux.HandleFunc) refers
	// to the assembler's own variable, not the constructor. The constructor
	// was directly emitted with its disambiguated name; no route was ever
	// written referencing the constructor by the pre-reserved name.
	//
	// This does NOT include non-stdlib import aliases or slot operator vars:
	// - Non-stdlib aliases: routes reference constructor variables by name,
	//   not import aliases (finding 23 removed import renames from routes).
	//   When a constructor collides with an import alias, the route still
	//   references the constructor by its original name and the rename MUST
	//   be applied to update the route.
	// - Slot operator vars: these never appear in route expressions.
	preReserved := make(map[string]bool)

	// Seed with slot operator variable names so constructor vars are
	// disambiguated against them (they share the same function scope).
	for name := range slotVarNames {
		varNames[name]++
		varUsed[name] = true
	}

	// Seed with assembler-internal template variables (mux, addr, db, dsn,
	// err) and standard library import aliases (log, fmt, http, os, sql)
	// that are emitted by writeDBSetup, writeRouteRegistration, and
	// writeServerStart into the same function scope.
	for name := range assemblerInternalVars(hasDB, hasRoutes) {
		varNames[name]++
		varUsed[name] = true
		preReserved[name] = true
	}

	// Seed with non-stdlib import aliases (component, fill, and slots
	// aliases) assigned during writeMainImports. A constructor variable
	// with the same name (e.g. cache.NewStorage() → "storage") would
	// shadow the import alias from its declaration point onward, causing
	// later constructor expressions referencing that import to resolve to
	// the local variable instead — producing an unused-import compile error
	// or wrong-package reference.
	for name := range imports.NonStdlibAliases {
		varNames[name]++
		varUsed[name] = true
	}

	// Flatten all constructors into a single list with dependency metadata.
	var entries []constructorEntry
	for i, cw := range input.Wirings {
		if cw.Wiring == nil {
			continue
		}
		for j, constructor := range cw.Wiring.Constructors {
			var deps []string
			if cw.Wiring.ConstructorDeps != nil {
				deps = cw.Wiring.ConstructorDeps[j]
			}
			entries = append(entries, constructorEntry{
				WiringIndex:      i,
				ConstructorIndex: j,
				RawExpr:          constructor,
				BaseVar:          rawConstructorVarName(constructor),
				Deps:             deps,
			})
		}
	}

	// Topologically sort constructors so that each variable is declared
	// before any constructor that references it. This is necessary because
	// the archetype's component order is conceptual — it does not encode
	// dependency information.
	sorted, err := topoSortConstructors(entries)
	if err != nil {
		return nil, err
	}

	for _, entry := range sorted {
		// Skip constructors that have no downstream consumer (no route
		// references them, they are not the middleware, and no consumed
		// constructor depends on them). Emitting an unused constructor
		// variable would produce a Go compile error.
		key := constructorKey{WiringIndex: entry.WiringIndex, ConstructorIndex: entry.ConstructorIndex}
		if consumed != nil && !consumed[key] {
			continue
		}

		// Apply import alias renames to the constructor expression so
		// that package-qualified references match the disambiguated
		// import aliases (e.g. "models.NewBar()" → "models2.NewBar()").
		resolvedExpr := entry.RawExpr
		if renames, ok := imports.Renames[entry.WiringIndex]; ok {
			for oldBase, newAlias := range renames {
				resolvedExpr = replaceIdentRef(resolvedExpr, oldBase, newAlias)
			}
		}

		varName := disambiguateAlias(entry.BaseVar, varNames, varUsed)

		if varName != entry.BaseVar {
			wiringRenames[entry.WiringIndex] = append(wiringRenames[entry.WiringIndex], constructorRename{
				ConstructorIndex: entry.ConstructorIndex,
				OriginalVar:      entry.BaseVar,
				FinalVar:         varName,
				PreReserved:      preReserved[entry.BaseVar],
			})
		}

		// Inject slot operators into handler constructors using
		// structured metadata rather than naming convention matching.
		expr := resolvedExpr
		cw := input.Wirings[entry.WiringIndex]
		if entity, ok := cw.Wiring.ConstructorEntities[entry.ConstructorIndex]; ok && entity != "" {
			if slotVars, ok := slotVarsByEntity[entity]; ok && len(slotVars) > 0 {
				expr = injectConstructorArgs(resolvedExpr, slotVars)
			}
		}

		fmt.Fprintf(buf, "\t%s := %s\n", varName, expr)
	}
	buf.WriteString("\n")
	return wiringRenames, nil
}

// topoSortConstructors topologically sorts constructor entries so that
// each constructor is emitted after all constructors whose variables it
// depends on. Returns an error if a cycle is detected.
func topoSortConstructors(entries []constructorEntry) ([]constructorEntry, error) {
	if len(entries) == 0 {
		return entries, nil
	}

	// Build a map from base variable name to entry index. When multiple
	// entries have the same base var name (which is handled by disambiguation),
	// the first one is the canonical producer of that variable name.
	varToIdx := make(map[string]int)
	for i, e := range entries {
		if _, exists := varToIdx[e.BaseVar]; !exists {
			varToIdx[e.BaseVar] = i
		}
	}

	// Build adjacency list: edges[i] = list of indices that entry i depends on
	// (i.e., must be emitted before entry i).
	n := len(entries)
	edges := make([][]int, n)    // edges[i] = dependencies of i
	inDegree := make([]int, n)

	for i, e := range entries {
		for _, dep := range e.Deps {
			if j, ok := varToIdx[dep]; ok && j != i {
				edges[i] = append(edges[i], j)
				inDegree[i]++ // i depends on j, so i has one more incoming edge
			}
			// If dep is not produced by any constructor (e.g. "db" from
			// writeDBSetup), it's an external dependency — no ordering needed.
		}
	}

	// Kahn's algorithm for topological sort.
	// Queue entries with zero in-degree (no dependencies).
	var queue []int
	for i := 0; i < n; i++ {
		if inDegree[i] == 0 {
			queue = append(queue, i)
		}
	}

	// Build reverse adjacency for "who depends on me?" to decrement in-degree.
	revEdges := make([][]int, n)
	for i, deps := range edges {
		for _, j := range deps {
			revEdges[j] = append(revEdges[j], i)
		}
	}

	var sorted []constructorEntry
	for len(queue) > 0 {
		idx := queue[0]
		queue = queue[1:]
		sorted = append(sorted, entries[idx])

		// Decrement in-degree of all entries that depend on this one.
		for _, dependent := range revEdges[idx] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	if len(sorted) != n {
		// Cycle detected — find the entries involved.
		var cycleEntries []string
		for i := 0; i < n; i++ {
			if inDegree[i] > 0 {
				cycleEntries = append(cycleEntries, fmt.Sprintf("%s (deps: %v)", entries[i].RawExpr, entries[i].Deps))
			}
		}
		return nil, fmt.Errorf("circular constructor dependency detected among: %s", strings.Join(cycleEntries, "; "))
	}

	return sorted, nil
}

func writeSlotWiring(buf *bytes.Buffer, input AssemblerInput, slotVarsByEntity map[string][]string, hasDB bool, consumedWirings map[int]bool) {
	buf.WriteString("\t// Slot wiring — fills composed via operators.\n")

	// Compute fill import aliases (must match writeMainImports exactly).
	// hasDB is passed from the caller (effectiveHasDB from
	// computeConsumedConstructors) to ensure stdlib alias seeding matches
	// writeMainImports exactly — see checklist item 121. consumedWirings
	// ensures component import filtering matches writeMainImports — see
	// finding 30.
	fillAliasMap := buildFillAliasMap(input, hasDB, consumedWirings)

	// Build a set of operator variable names that will be injected into
	// handler constructors. Variables NOT in this set need `_ =` to prevent
	// unused variable errors (e.g. when a slot has no entity association).
	injected := make(map[string]bool)
	for _, vars := range slotVarsByEntity {
		for _, v := range vars {
			injected[v] = true
		}
	}

	for _, sb := range input.SlotBindings {
		slotPascal := snakeToPascal(sb.Slot)
		entityComment := ""
		if sb.Entity != "" {
			entityComment = fmt.Sprintf(" for %s", sb.Entity)
		}

		if len(sb.Gate) > 0 {
			varName := slotVarName(sb.Slot, sb.Entity, "Gate")
			fmt.Fprintf(buf, "\t// Slot: %s (gate)%s\n", sb.Slot, entityComment)
			fmt.Fprintf(buf, "\t%s := slots.New%sGate(\n", varName, slotPascal)
			for _, fillName := range sb.Gate {
				alias := fillAliasMap[fillName]
				fmt.Fprintf(buf, "\t\t%s.New(),\n", alias)
			}
			fmt.Fprintf(buf, "\t)\n")
			if !injected[varName] {
				fmt.Fprintf(buf, "\t_ = %s\n", varName)
			}
			buf.WriteString("\n")
		}

		if len(sb.Chain) > 0 {
			varName := slotVarName(sb.Slot, sb.Entity, "Chain")
			scStr := "false"
			if sb.ShortCircuit {
				scStr = "true"
			}
			fmt.Fprintf(buf, "\t// Slot: %s (chain, short_circuit=%s)%s\n", sb.Slot, scStr, entityComment)
			fmt.Fprintf(buf, "\t%s := slots.New%sChain(%s,\n", varName, slotPascal, scStr)
			for _, fillName := range sb.Chain {
				alias := fillAliasMap[fillName]
				fmt.Fprintf(buf, "\t\t%s.New(),\n", alias)
			}
			fmt.Fprintf(buf, "\t)\n")
			if !injected[varName] {
				fmt.Fprintf(buf, "\t_ = %s\n", varName)
			}
			buf.WriteString("\n")
		}

		if len(sb.FanOut) > 0 {
			varName := slotVarName(sb.Slot, sb.Entity, "FanOut")
			fmt.Fprintf(buf, "\t// Slot: %s (fan-out)%s\n", sb.Slot, entityComment)
			fmt.Fprintf(buf, "\t%s := slots.New%sFanOut(\n", varName, slotPascal)
			for _, fillName := range sb.FanOut {
				alias := fillAliasMap[fillName]
				fmt.Fprintf(buf, "\t\t%s.New(),\n", alias)
			}
			fmt.Fprintf(buf, "\t)\n")
			if !injected[varName] {
				fmt.Fprintf(buf, "\t_ = %s\n", varName)
			}
			buf.WriteString("\n")
		}
	}
}

func writeRouteRegistration(buf *bytes.Buffer, input AssemblerInput, wiringRenames map[int][]constructorRename) {
	buf.WriteString("\tmux := http.NewServeMux()\n")
	for i, cw := range input.Wirings {
		if cw.Wiring == nil {
			continue
		}
		for _, route := range cw.Wiring.Routes {
			// Route expressions reference constructor variables (e.g.
			// "userHandler.Create"), not packages. Only constructor variable
			// renames are applied — import alias renames must NOT be applied
			// to routes because they would corrupt variable references when
			// a constructor's derived variable name matches an import base
			// name (finding 23: multi-pass rename interference).
			updatedRoute := applyConstructorRenames(route, wiringRenames[i])
			fmt.Fprintf(buf, "\t%s\n", updatedRoute)
		}
	}
	buf.WriteString("\n")
}

// applyConstructorRenames replaces variable references in a route expression
// when constructor variable names were disambiguated. Each rename is applied
// independently using word-boundary-safe replacement.
//
// Renames where PreReserved is true are skipped — these arose from collision
// with a pre-reserved identifier (assembler-internal vars, import aliases,
// slot operator vars), meaning the constructor was never named OriginalVar in
// the generated code. Applying such a rename to routes would corrupt
// references to the pre-reserved identifier (e.g. turning mux.HandleFunc
// into mux2.HandleFunc when "mux" was reserved by writeRouteRegistration).
func applyConstructorRenames(route string, renames []constructorRename) string {
	for _, r := range renames {
		if r.PreReserved {
			continue
		}
		route = replaceIdentRef(route, r.OriginalVar, r.FinalVar)
	}
	return route
}

// replaceIdentRef replaces occurrences of oldName followed by a dot with
// newName followed by a dot, but only at identifier word boundaries. This
// prevents "store." from matching within "datastore.".
func replaceIdentRef(s, oldName, newName string) string {
	if oldName == newName {
		return s
	}
	target := oldName + "."
	var result strings.Builder
	i := 0
	for i < len(s) {
		idx := strings.Index(s[i:], target)
		if idx < 0 {
			result.WriteString(s[i:])
			break
		}
		absIdx := i + idx
		// Check word boundary: character before match must not be an identifier char.
		if absIdx > 0 && isIdentChar(s[absIdx-1]) {
			// Not at a word boundary — copy through this non-match and continue.
			result.WriteString(s[i : absIdx+len(target)])
			i = absIdx + len(target)
			continue
		}
		// Word boundary match — replace.
		result.WriteString(s[i:absIdx])
		result.WriteString(newName + ".")
		i = absIdx + len(target)
	}
	return result.String()
}

// isIdentChar returns true if b is a valid Go identifier character.
func isIdentChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// containsBareIdent checks whether s contains the identifier name as a
// standalone token (surrounded by non-identifier characters). Unlike
// containsIdentRef which looks for "name." (method/field access), this
// looks for the bare identifier — e.g. "store" inside "(store, cache)".
func containsBareIdent(s, name string) bool {
	if len(name) == 0 {
		return false
	}
	for i := 0; i <= len(s)-len(name); {
		idx := strings.Index(s[i:], name)
		if idx < 0 {
			return false
		}
		absIdx := i + idx
		endIdx := absIdx + len(name)
		leftOk := absIdx == 0 || !isIdentChar(s[absIdx-1])
		rightOk := endIdx >= len(s) || !isIdentChar(s[endIdx])
		if leftOk && rightOk {
			return true
		}
		i = absIdx + 1
	}
	return false
}

func writeServerStart(buf *bytes.Buffer, input AssemblerInput, wiringRenames map[int][]constructorRename) {
	// Check if there's an auth middleware to wrap the mux.
	middlewareVar, wrapExpr, wiringIdx, constructorIdx := findMiddlewareVar(input)
	// Apply disambiguation renames to the middleware variable reference,
	// matching by constructor index to avoid misdirection when multiple
	// constructors in the same wiring share the same base variable name.
	for _, r := range wiringRenames[wiringIdx] {
		if r.ConstructorIndex == constructorIdx {
			middlewareVar = r.FinalVar
			break
		}
	}

	buf.WriteString("\taddr := fmt.Sprintf(\":%d\", ")
	fmt.Fprintf(buf, "%d)\n", input.Port)
	buf.WriteString("\tlog.Printf(\"starting server on %%s\", addr)\n")

	if middlewareVar != "" {
		// Use the generator-provided wrap expression to invoke the middleware.
		// The expression is a format string with two %s verbs: middleware var
		// and handler var (e.g. "%s(%s)" produces "authMiddleware(mux)").
		wrappedHandler := fmt.Sprintf(wrapExpr, middlewareVar, "mux")
		fmt.Fprintf(buf, "\tlog.Fatal(http.ListenAndServe(addr, %s))\n", wrappedHandler)
	} else {
		buf.WriteString("\tlog.Fatal(http.ListenAndServe(addr, mux))\n")
	}
}

// hasAnyRoutes returns true if any component wiring has routes.
func hasAnyRoutes(input AssemblerInput) bool {
	for _, cw := range input.Wirings {
		if cw.Wiring != nil && len(cw.Wiring.Routes) > 0 {
			return true
		}
	}
	return false
}

// findMiddlewareVar returns the variable name of an auth middleware
// constructor, the wrap expression, the wiring index it belongs to, and
// the constructor index within that wiring.
// Returns ("", "", -1, -1) if none exists.
// Uses structured MiddlewareConstructor metadata rather than string matching.
func findMiddlewareVar(input AssemblerInput) (string, string, int, int) {
	for i, cw := range input.Wirings {
		if cw.Wiring == nil || cw.Wiring.MiddlewareConstructor == nil {
			continue
		}
		idx := *cw.Wiring.MiddlewareConstructor
		if idx >= 0 && idx < len(cw.Wiring.Constructors) {
			return rawConstructorVarName(cw.Wiring.Constructors[idx]), cw.Wiring.MiddlewareWrapExpr, i, idx
		}
	}
	return "", "", -1, -1
}

// rawConstructorVarName derives a base Go variable name from a constructor expression.
// "pkg.NewFooBar(args)" → "fooBar"
// "pkg.NewStore(db)" → "store"
func rawConstructorVarName(expr string) string {
	// Extract the function name after the last dot before '('.
	funcName := expr
	if dotIdx := strings.LastIndex(expr, "."); dotIdx >= 0 {
		funcName = expr[dotIdx+1:]
	}
	if parenIdx := strings.Index(funcName, "("); parenIdx >= 0 {
		funcName = funcName[:parenIdx]
	}

	// Strip "New" prefix.
	if strings.HasPrefix(funcName, "New") {
		funcName = funcName[3:]
	}

	if len(funcName) == 0 {
		return "v"
	}

	// Lower first letter for camelCase.
	return strings.ToLower(funcName[:1]) + funcName[1:]
}

// rawFillImportAlias converts a fill name (e.g. "admin-creation-policy") to a
// base Go import alias (e.g. "admincreationpolicy") before disambiguation.
func rawFillImportAlias(fillName string) string {
	s := strings.ReplaceAll(fillName, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	return strings.ToLower(s)
}

// disambiguateAlias returns a unique alias given a base alias. If the base
// alias has been seen before, a numeric suffix is appended (e.g. "store2").
// The counts and used maps are mutated to track state across calls.
func disambiguateAlias(base string, counts map[string]int, used map[string]bool) string {
	counts[base]++
	if counts[base] == 1 {
		used[base] = true
		return base
	}
	alias := fmt.Sprintf("%s%d", base, counts[base])
	for used[alias] {
		counts[base]++
		alias = fmt.Sprintf("%s%d", base, counts[base])
	}
	used[alias] = true
	return alias
}

// injectConstructorArgs inserts additional arguments into a constructor
// expression before its closing parenthesis.
func injectConstructorArgs(expr string, args []string) string {
	if len(args) == 0 {
		return expr
	}
	closeIdx := strings.LastIndex(expr, ")")
	if closeIdx < 0 {
		return expr
	}
	openIdx := strings.Index(expr, "(")
	if openIdx < 0 {
		return expr
	}

	prefix := expr[:closeIdx]
	suffix := expr[closeIdx:]

	inner := strings.TrimSpace(expr[openIdx+1 : closeIdx])
	if inner == "" {
		return prefix + strings.Join(args, ", ") + suffix
	}
	return prefix + ", " + strings.Join(args, ", ") + suffix
}

// buildSlotVarsByEntity returns a map from entity name to a list of slot
// operator variable names for that entity. These variables are created by
// writeSlotWiring and injected into handler constructors by writeConstructors.
func buildSlotVarsByEntity(bindings []types.SlotDeclaration, hasSlots bool) map[string][]string {
	result := make(map[string][]string)
	if !hasSlots {
		return result
	}
	for _, sb := range bindings {
		if sb.Entity == "" {
			continue
		}
		if len(sb.Gate) > 0 {
			result[sb.Entity] = append(result[sb.Entity], slotVarName(sb.Slot, sb.Entity, "Gate"))
		}
		if len(sb.Chain) > 0 {
			result[sb.Entity] = append(result[sb.Entity], slotVarName(sb.Slot, sb.Entity, "Chain"))
		}
		if len(sb.FanOut) > 0 {
			result[sb.Entity] = append(result[sb.Entity], slotVarName(sb.Slot, sb.Entity, "FanOut"))
		}
	}
	return result
}

// buildFillAliasMap returns a map from fill name to its resolved import alias.
// Must replay the same unified disambiguation sequence as writeMainImports:
// slots alias first, then component import aliases, then fill aliases.
// hasDB must be the same effectiveHasDB value used by writeMainImports so that
// stdlib alias seeding is identical — see checklist item 121.
func buildFillAliasMap(input AssemblerInput, hasDB bool, consumedWirings map[int]bool) map[string]string {
	aliases := make(map[string]int)
	aliasUsed := make(map[string]bool)
	seen := make(map[string]bool)

	// Seed stdlib aliases (must match writeMainImports exactly).
	hasRoutes := hasAnyRoutes(input)
	for _, name := range stdlibAliases(hasRoutes, hasDB) {
		aliases[name]++
		aliasUsed[name] = true
	}

	// Replay slots alias registration.
	hasSlots := len(input.SlotBindings) > 0 && input.SlotsPackage != ""
	if hasSlots {
		fullPath := generatedImportPath(input.ModuleName, input.OutDirName, input.SlotsPackage)
		if !seen[fullPath] {
			seen[fullPath] = true
			disambiguateAlias("slots", aliases, aliasUsed)
		}
	}

	// Replay component import alias registration — only for consumed
	// wirings, matching writeMainImports filtering (finding 30).
	for i, cw := range input.Wirings {
		if cw.Wiring == nil {
			continue
		}
		if !consumedWirings[i] {
			continue
		}
		for _, imp := range cw.Wiring.Imports {
			fullPath := generatedImportPath(input.ModuleName, input.OutDirName, imp)
			if seen[fullPath] {
				continue
			}
			seen[fullPath] = true
			disambiguateAlias(path.Base(imp), aliases, aliasUsed)
		}
	}

	// Now compute fill aliases using the same namespace.
	fillNames := collectFillNames(input.SlotBindings)
	result := make(map[string]string, len(fillNames))
	for _, name := range fillNames {
		baseAlias := rawFillImportAlias(name)
		alias := disambiguateAlias(baseAlias, aliases, aliasUsed)
		result[name] = alias
	}
	return result
}

// collectFillNames returns a deduplicated, sorted list of fill names from
// all slot bindings.
func collectFillNames(bindings []types.SlotDeclaration) []string {
	seen := make(map[string]bool)
	var names []string
	for _, sb := range bindings {
		for _, n := range sb.Gate {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
		for _, n := range sb.Chain {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
		for _, n := range sb.FanOut {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	return names
}

// snakeToPascal converts a snake_case string to PascalCase.
// "before_create" → "BeforeCreate"
func snakeToPascal(s string) string {
	parts := strings.Split(s, "_")
	var result strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		result.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return result.String()
}

// snakeToCamel converts a snake_case string to camelCase.
// "before_create" → "beforeCreate"
func snakeToCamel(s string) string {
	pascal := snakeToPascal(s)
	if len(pascal) == 0 {
		return ""
	}
	return strings.ToLower(pascal[:1]) + pascal[1:]
}

// validateConstructorUniqueness checks that within each wiring, no two
// constructors derive the same base variable name. Intra-wiring name
// collisions make route-to-constructor binding ambiguous — the assembler
// cannot determine which constructor a route expression references when
// multiple constructors share the same derived variable name.
func validateConstructorUniqueness(wirings []ComponentWiring) error {
	for _, cw := range wirings {
		if cw.Wiring == nil {
			continue
		}
		seen := make(map[string]string) // base var name → first constructor expression
		for _, constructor := range cw.Wiring.Constructors {
			baseVar := rawConstructorVarName(constructor)
			if first, ok := seen[baseVar]; ok {
				return fmt.Errorf("component %q has multiple constructors deriving variable name %q: %q and %q — constructor names within a component must be distinct",
					cw.Name, baseVar, first, constructor)
			}
			seen[baseVar] = constructor
		}
	}
	return nil
}

// validateSlotBindingUniqueness checks that no two slot bindings share the same
// (slot, entity, operator) composite key. Duplicate bindings would produce
// duplicate variable declarations in the generated main.go.
func validateSlotBindingUniqueness(bindings []types.SlotDeclaration) error {
	type compositeKey struct {
		slot, entity, operator string
	}
	seen := make(map[compositeKey]bool)

	for _, sb := range bindings {
		operators := []struct {
			name string
			has  bool
		}{
			{"gate", len(sb.Gate) > 0},
			{"chain", len(sb.Chain) > 0},
			{"fan-out", len(sb.FanOut) > 0},
		}
		for _, op := range operators {
			if !op.has {
				continue
			}
			key := compositeKey{slot: sb.Slot, entity: sb.Entity, operator: op.name}
			if seen[key] {
				entityDesc := ""
				if sb.Entity != "" {
					entityDesc = fmt.Sprintf(" for entity %q", sb.Entity)
				}
				return fmt.Errorf("duplicate slot binding: slot %q%s with operator %q appears more than once", sb.Slot, entityDesc, op.name)
			}
			seen[key] = true
		}
	}
	return nil
}

// stdlibAliases returns the set of implicit package-name aliases introduced by
// conditionally-imported standard library packages. These must be seeded into
// import alias disambiguation maps to prevent non-stdlib imports from shadowing
// them (e.g. component "internal/sql" getting alias "sql" and shadowing
// "database/sql").
func stdlibAliases(hasRoutes, hasDB bool) []string {
	var names []string
	if hasDB {
		names = append(names, "sql", "os")
	}
	if hasRoutes {
		names = append(names, "fmt", "http")
	}
	if hasDB || hasRoutes {
		names = append(names, "log")
	}
	return names
}

// validateSlotVarNameUniqueness checks that no two slot bindings produce the
// same derived variable name. Distinct raw composite keys (slot, entity, operator)
// can normalize to the same camelCase identifier when the slot names differ only
// in underscore structure (e.g. "before_create" and "before__create" both produce
// "beforeCreate" via snakeToCamel). This would produce duplicate := declarations
// in the generated main.go.
func validateSlotVarNameUniqueness(bindings []types.SlotDeclaration) error {
	type varSource struct {
		slot, entity, operator string
	}
	seen := make(map[string]varSource) // derived var name → first source

	for _, sb := range bindings {
		operators := []struct {
			name   string
			suffix string
			has    bool
		}{
			{"gate", "Gate", len(sb.Gate) > 0},
			{"chain", "Chain", len(sb.Chain) > 0},
			{"fan-out", "FanOut", len(sb.FanOut) > 0},
		}
		for _, op := range operators {
			if !op.has {
				continue
			}
			varName := slotVarName(sb.Slot, sb.Entity, op.suffix)
			source := varSource{slot: sb.Slot, entity: sb.Entity, operator: op.name}
			if first, ok := seen[varName]; ok {
				// Only report if the raw composite keys differ — if they're
				// identical, validateSlotBindingUniqueness already catches it.
				if first.slot != source.slot || first.entity != source.entity || first.operator != source.operator {
					return fmt.Errorf("slot bindings %q (entity %q, operator %q) and %q (entity %q, operator %q) produce the same variable name %q — slot names must be distinct after normalization",
						first.slot, first.entity, first.operator,
						source.slot, source.entity, source.operator,
						varName)
				}
			}
			seen[varName] = source
		}
	}
	return nil
}

// generatedImportPath constructs a full Go import path for a generated package.
// With go.mod at the project root, generated packages live under the output
// directory (e.g. "out/internal/api") and their import paths must include the
// output directory name. If outDirName is empty, no prefix is added.
func generatedImportPath(moduleName, outDirName, relativePath string) string {
	if outDirName != "" {
		return moduleName + "/" + outDirName + "/" + relativePath
	}
	return moduleName + "/" + relativePath
}

// slotVarName derives a unique slot operator variable name from the slot name,
// entity name (may be empty), and operator type suffix ("Gate", "Chain", "FanOut").
// When an entity is set, the entity name is included to disambiguate per-entity
// bindings of the same slot (e.g. "beforeCreateUserGate" vs "beforeCreateOrgGate").
func slotVarName(slot, entity, operatorSuffix string) string {
	base := snakeToCamel(slot)
	if entity != "" {
		base += entity
	}
	return base + operatorSuffix
}

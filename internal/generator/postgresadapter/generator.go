// Package postgresadapter implements the postgres-adapter component Generator. It gives the database opener of a
// service and, for a service with entities, the tables of the runtime data path as SQL from the declaration, the
// migration ledger that applies them and the schema generation marker (tables.go, ST-SUP-RC01-OC41). It emits no
// store and no entity model: the generated commands and queries carry all data access, on the only data path
// (ST-SUP-RC20-OC39).
package postgresadapter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// validFieldNamePattern defines the safe character set for field names across
// all target systems (Go identifiers, SQL identifiers, URL segments).
var validFieldNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Generator produces the postgres-adapter component's generated code.
type Generator struct{}

// MinimumGoVersion covers the compiler minimum for golang.org/x/text.
func (*Generator) MinimumGoVersion() string { return "1.25.0" }

// ValidateContext checks storage names, constraints, and migration settings, with the checks of Generate.
func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := validateTenantColumn(ctx); err != nil {
		return err
	}
	if _, err := schemaGeneration(ctx); err != nil {
		return err
	}
	if err := validateWriterLease(ctx); err != nil {
		return err
	}
	if peer := ctx.PeerNamespaces["otel-tracing"]; peer != "" {
		if err := gen.ValidateGoPackageNamespace(peer); err != nil {
			return err
		}
	}
	if errs := types.ValidateVersioned(ctx.Entities, ctx.Collections); len(errs) > 0 {
		return errs[0]
	}
	if errs := types.ValidateLiveUnique(ctx.Entities, ctx.Collections); len(errs) > 0 {
		return errs[0]
	}
	migrations := "startup"
	if value, present := ctx.ComponentConfig["migrations"]; present {
		var ok bool
		migrations, ok = value.(string)
		if !ok || (migrations != "startup" && migrations != "external") {
			return fmt.Errorf("migrations must be startup or external")
		}
	}
	if len(ctx.Entities) == 0 {
		return gen.ValidateGoPackageNamespace(ctx.OutputNamespace)
	}
	// Validate no duplicate entity names.
	if err := validateEntityUniqueness(ctx.Entities); err != nil {
		return err
	}

	// Validate no case-insensitive entity name collisions (would produce
	// the same table name).
	if err := validateCaseInsensitiveUniqueness(ctx.Entities); err != nil {
		return err
	}

	// Validate entity names don't collide with generator-internal identifiers.
	if err := validateReservedNames(ctx.Entities); err != nil {
		return err
	}

	// Validate that ref fields' "to" attributes reference existing entities.
	if err := validateRefTargets(ctx.Entities); err != nil {
		return err
	}

	// Validate no duplicate field names within any entity.
	if err := validateFieldUniqueness(ctx.Entities); err != nil {
		return err
	}

	// Validate entity names use a safe character set for all target systems.
	if err := validateEntityNameCharset(ctx.Entities); err != nil {
		return err
	}

	// Validate field names use a safe character set for all target systems.
	if err := validateFieldNameCharset(ctx.Entities); err != nil {
		return err
	}

	// Validate no field name collides with the implicit "id" primary key.
	if err := validateNoImplicitIDCollision(ctx.Entities); err != nil {
		return err
	}

	// Validate derived PascalCase field names are unique within each entity.
	if err := validateDerivedFieldUniqueness(ctx.Entities); err != nil {
		return err
	}

	// Validate derived entity names are valid, usable Go type names.
	if err := validateDerivedEntityValidity(ctx.Entities); err != nil {
		return err
	}

	// Validate derived PascalCase field names are valid Go identifiers.
	if err := validateDerivedFieldValidity(ctx.Entities); err != nil {
		return err
	}

	// Validate that enum fields have non-empty values.
	if err := validateEnumValues(ctx.Entities); err != nil {
		return err
	}

	for _, entity := range ctx.Entities {
		for _, field := range entity.Fields {
			if field.Min == nil && field.Max == nil {
				continue
			}
			switch field.Type {
			case types.FieldTypeInt32, types.FieldTypeInt64, types.FieldTypeFloat, types.FieldTypeDouble:
			default:
				return fmt.Errorf("entity %s field %s: numeric bounds require a numeric type", entity.Name, field.Name)
			}
			for _, bound := range []*float64{field.Min, field.Max} {
				if bound != nil && (math.IsNaN(*bound) || math.IsInf(*bound, 0)) {
					return fmt.Errorf("entity %s field %s: numeric bounds must be finite", entity.Name, field.Name)
				}
			}
			if field.Min != nil && field.Max != nil && *field.Min > *field.Max {
				return fmt.Errorf("entity %s field %s: numeric range is empty", entity.Name, field.Name)
			}
		}
	}
	if _, err := tableOrder(ctx.Entities); err != nil {
		return err
	}
	if err := validateRuntimeData(ctx); err != nil {
		return err
	}

	return gen.ValidateGoPackageNamespace(ctx.OutputNamespace)
}

// Generate gives the files and the wiring of the component. A service with entities gets the tables of the runtime
// data path (generateRuntimeData, ST-SUP-RC01-OC41), which is the only data path (ST-SUP-RC20-OC39); a service with
// no entity has no data access and gets the database opener alone.
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	if len(ctx.Entities) == 0 {
		file, err := generateDatabaseOpener(ctx)
		if err != nil {
			return nil, nil, err
		}
		wiring := &gen.Wiring{NeedsDB: true, Imports: []string{ctx.OutputNamespace}, DatabaseOpener: &gen.DatabaseOpenerSpec{Namespace: ctx.OutputNamespace, Function: "OpenDatabase"}, GoModRequires: map[string]string{"github.com/jackc/pgx/v5": "v5.11.0"}}
		return []gen.File{file}, wiring, nil
	}
	return generateRuntimeData(ctx)
}

// entityAccess adds the table of each entity to the database access that the runtime role is granted and to the
// backup manifest. The runtime role holds only the privileges of ST-SUP-RC27-OC07 and ST-STD-TXN-R03.
func entityAccess(ctx gen.Context, wiring *gen.Wiring) {
	for _, entity := range ctx.Entities {
		wiring.DatabaseAccess = append(wiring.DatabaseAccess, gen.DatabaseObject{
			Schema: "public", Name: tableName(entity.Name), Kind: "table",
			Privileges: runtimePrivileges(ctx, entity),
		})
		wiring.BackupObjects = append(wiring.BackupObjects, gen.BackupObject{
			Schema: "public", Name: tableName(entity.Name), Kind: "table",
		})
	}
}

// schemaGenerationAccess adds the objects of the schema generation marker and of the migration ledger to the
// database access and to the backup manifest, when the service declares schema_generation.
func schemaGenerationAccess(ctx gen.Context, wiring *gen.Wiring) {
	if ctx.ComponentConfig["schema_generation"] == nil {
		return
	}
	wiring.DatabaseAccess = append(wiring.DatabaseAccess, gen.DatabaseObject{
		Schema: "stego_schema", Name: "generation", Kind: "table", Privileges: []string{"SELECT"},
	})
	wiring.DatabaseAccess = append(wiring.DatabaseAccess, gen.DatabaseObject{
		Schema: "stego_schema", Name: "identity", Kind: "table", Privileges: []string{"SELECT"},
	})
	wiring.DatabaseAccess = append(wiring.DatabaseAccess, gen.DatabaseObject{
		Schema: "stego_schema", Name: "epoch_seq", Kind: "sequence", Privileges: []string{"SELECT", "USAGE"},
	})
	wiring.BackupObjects = append(wiring.BackupObjects,
		gen.BackupObject{Schema: "stego_schema", Name: "generation", Kind: "table"},
		gen.BackupObject{Schema: "stego_schema", Name: "identity", Kind: "table"},
		gen.BackupObject{Schema: "stego_schema", Name: "epoch_seq", Kind: "sequence"},
		gen.BackupObject{Schema: "stego_schema", Name: "migrations", Kind: "table"},
	)
}

// reservedTypeNames is the union of: (1) Go keywords, (2) Go predeclared
// identifiers, and (3) generator-internal identifiers. Entity names that
// match any of these produce uncompilable or shadowed generated code.
var reservedTypeNames = map[string]bool{
	"SchemaGeneration": true, "SchemaDefinition": true, "ErrSchemaGeneration": true, "VerifySchema": true, "BootstrapSchema": true, "readSchemaGeneration": true,
	"databaseUUID":       true,
	"ErrMigrationLedger": true, "AppliedMigrations": true, "ApplyMigration": true, "RegisterSQL": true,
	"Database": true, "inTransaction": true, "lockLedger": true, "recordMigration": true,
	// Generator-internal identifiers.
	"stegotenancy":             true,
	"OpenDatabase":             true,
	"databaseConnector":        true,
	"databaseLoopbackTestMode": true,
	"databaseTransport":        true,
	"tls":                      true,
	"net":                      true,
	"driver":                   true,
	"databasePoolSettings":     true,
	"readDatabasePoolSettings": true,
	"databasePoolInteger":      true,
	"databasePoolDuration":     true,
	"os":                       true,
	"strconv":                  true,
	"databaseConfiguration":    true,
	"databaseTracer":           true,
	"databaseCallKey":          true,
	"databaseCall":             true,
	"beginDatabaseCall":        true,
	"endDatabaseCall":          true,
	"pgx":                      true,
	"stdlib":                   true,
	"telemetry":                true,
	"context":                  true,
	"errors":                   true,

	"Migrate": true,
	// Go keywords.
	"break": true, "case": true, "chan": true, "const": true,
	"continue": true, "default": true, "defer": true, "else": true,
	"fallthrough": true, "for": true, "func": true, "go": true,
	"goto": true, "if": true, "import": true, "interface": true,
	"map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true,
	"var": true,
	// Go predeclared identifiers.
	"any": true, "bool": true, "byte": true, "comparable": true,
	"complex64": true, "complex128": true, "error": true, "float32": true,
	"float64": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "rune": true, "string": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true,
	"uint64": true, "uintptr": true,
	"true": true, "false": true, "iota": true, "nil": true,
	"append": true, "cap": true, "clear": true, "close": true,
	"complex": true, "copy": true, "delete": true, "imag": true,
	"len": true, "make": true, "max": true, "min": true,
	"new": true, "panic": true, "print": true, "println": true,
	"real": true, "recover": true,
	// Import aliases used in generated files.
	"fmt":  true,
	"sql":  true,
	"time": true,
	"uuid": true,
}

// --- Migration ---

// migrationLedger is the source of the migration ledger in migrate.go, after its imports. It runs on database/sql
// (Database of schema_generation.go): each migration and its ledger row commit in one transaction, under the ledger
// lock, after a read of the ledger in that transaction, so a migration applies once and in the order of its name
// (ST-SUP-RC01-OC11).
const migrationLedger = `// MigrationFunc is a function that performs a database migration. It runs in the
// transaction that records the migration in the ledger, and must not commit it.
type MigrationFunc func(ctx context.Context, tx *sql.Tx) error

// Migration represents a named database migration. The digest covers the
// migration body; an edited migration changes the digest and is rejected.
type Migration struct {
	Name   string
	Digest string
	Func   MigrationFunc
}

var migrations []Migration

// Digest computes the ledger digest of a migration body. The body is the
// SQL text of a SQL migration and the generated source text of a function
// migration.
func Digest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// Register adds a function migration to the ordered migration list. The
// digest is Digest of the migration's source text; STEGO computes it for
// each migration it generates. Migration names must sort in apply order;
// use zero-padded number prefixes.
func Register(name, digest string, fn MigrationFunc) {
	migrations = append(migrations, Migration{Name: name, Digest: digest, Func: fn})
}

// RegisterSQL adds a SQL migration to the ordered migration list. The
// digest covers the SQL text, so an edit after application is detected.
func RegisterSQL(name, body string) {
	migrations = append(migrations, Migration{Name: name, Digest: Digest(body), Func: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, body)
		return err
	}})
}

// Migrate applies every registered migration and records each one in the
// stego_schema.migrations ledger. Applied versions must match the
// registered history exactly: a re-application, a version gap, an edited
// migration, and an unreadable ledger are all rejected. A fresh database
// applies all migrations inside the bootstrap transaction; afterwards
// each pending migration commits with its ledger row in one transaction.
func Migrate(ctx context.Context, db Database) error {
	if ctx == nil || db == nil {
		return fmt.Errorf("migration requires an initialized database")
	}
	ordered := orderedMigrations()
	if err := BootstrapSchema(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		for _, m := range ordered {
			if err := m.Func(ctx, tx); err != nil {
				return fmt.Errorf("migration %s: %w", m.Name, err)
			}
			if err := recordMigration(ctx, tx, m.Name, m.Digest); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return applyPending(ctx, db, ordered)
}

// ApplyMigration applies one SQL migration by body and records it in the
// ledger, for databases whose migrations run outside the application. The
// same continuity rules hold: no re-application, no gap, no digest change.
// The body runs with the ledger row in one transaction, so it must hold no
// transaction control (BEGIN, COMMIT, ROLLBACK, SAVEPOINT): a COMMIT ends the
// transaction early and leaves a partial migration with no ledger row. This
// is a stated limit; the body is not checked. The ledger then holds one row
// past the registered history, so a later ApplyMigration and Migrate are
// refused until a release registers that migration.
func ApplyMigration(ctx context.Context, db Database, name, body string) error {
	if ctx == nil || db == nil {
		return ErrMigrationLedger
	}
	ordered := orderedMigrations()
	for _, m := range ordered {
		if m.Name == name {
			return ErrMigrationLedger
		}
	}
	return inTransaction(ctx, db, &sql.TxOptions{Isolation: sql.LevelReadCommitted}, func(tx *sql.Tx) error {
		if err := lockLedger(ctx, tx); err != nil {
			return err
		}
		applied, err := appliedMigrations(ctx, tx)
		if err != nil {
			return err
		}
		if err := checkLedger(applied, ordered); err != nil {
			return err
		}
		if len(applied) < len(ordered) {
			return ErrMigrationLedger
		}
		if len(applied) > 0 && name <= applied[len(applied)-1].Name {
			return ErrMigrationLedger
		}
		if _, err := tx.ExecContext(ctx, body); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		return recordMigration(ctx, tx, name, Digest(body))
	})
}

// orderedMigrations returns the registered migrations sorted by name.
// Registration order follows package initialization; the apply order is
// the name order, so names must sort in dependency order.
func orderedMigrations() []Migration {
	ordered := make([]Migration, len(migrations))
	copy(ordered, migrations)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	return ordered
}

// recordMigration writes the ledger row of one migration in its transaction.
func recordMigration(ctx context.Context, tx *sql.Tx, name, digest string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO stego_schema.migrations(version,digest) VALUES($1,$2)", name, digest)
	return err
}

// appliedMigrations reads the ledger in apply order: the byte order of the
// version, as orderedMigrations sorts the names, whatever the collation of
// the database. It fails closed when the ledger is missing or unreadable.
func appliedMigrations(ctx context.Context, tx *sql.Tx) ([]Migration, error) {
	rows, err := tx.QueryContext(ctx, "SELECT version, digest FROM stego_schema.migrations ORDER BY version COLLATE \"C\"")
	if err != nil {
		return nil, ErrMigrationLedger
	}
	defer rows.Close()
	var applied []Migration
	for rows.Next() {
		var row Migration
		if err := rows.Scan(&row.Name, &row.Digest); err != nil {
			return nil, ErrMigrationLedger
		}
		applied = append(applied, row)
	}
	if rows.Err() != nil {
		return nil, ErrMigrationLedger
	}
	return applied, nil
}

// checkLedger rejects any mismatch between the applied versions and the
// registered history. Applied versions must be a prefix of the registered
// list, in order, with matching digests: no gaps, no extras, no edits.
func checkLedger(applied, registered []Migration) error {
	if len(applied) > len(registered) {
		return ErrMigrationLedger
	}
	for i, row := range applied {
		if row.Name != registered[i].Name || row.Digest != registered[i].Digest {
			return ErrMigrationLedger
		}
	}
	return nil
}

// applyPending applies the migrations after the recorded history, one
// transaction each, at read committed whatever the default isolation of the
// database is, so each statement takes its snapshot after the ledger lock is
// granted: it takes the ledger lock, reads the ledger again,
// refuses a ledger that is not a prefix of the registered history, and
// applies the next migration with its ledger row. A migration that another
// job applied in the meantime is not applied again.
func applyPending(ctx context.Context, db Database, ordered []Migration) error {
	for {
		done := false
		if err := inTransaction(ctx, db, &sql.TxOptions{Isolation: sql.LevelReadCommitted}, func(tx *sql.Tx) error {
			if err := lockLedger(ctx, tx); err != nil {
				return err
			}
			applied, err := appliedMigrations(ctx, tx)
			if err != nil {
				return err
			}
			if err := checkLedger(applied, ordered); err != nil {
				return err
			}
			if len(applied) == len(ordered) {
				done = true
				return nil
			}
			m := ordered[len(applied)]
			if err := m.Func(ctx, tx); err != nil {
				return fmt.Errorf("migration %s: %w", m.Name, err)
			}
			return recordMigration(ctx, tx, m.Name, m.Digest)
		}); err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

`

// generateMigrate produces migrate.go: the migration ledger in stego_schema.migrations, with 001_initial as the SQL
// migration of InitialTables (ST-SUP-RC01-OC41) and, in a pooled service, the row-level security of the tenancy
// component after it. The ledger refuses every row that is not the registered history, in order and with the body
// digest (ST-SUP-RC01-OC11).
func generateMigrate(ns string, rowSecurity string) (gen.File, error) {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "package %s\n\n", path.Base(ns))
	fmt.Fprintf(&buf, "import (\n")
	fmt.Fprintf(&buf, "\t\"context\"\n")
	fmt.Fprintf(&buf, "\t\"crypto/sha256\"\n")
	fmt.Fprintf(&buf, "\t\"database/sql\"\n")
	fmt.Fprintf(&buf, "\t\"encoding/hex\"\n")
	fmt.Fprintf(&buf, "\t\"fmt\"\n")
	fmt.Fprintf(&buf, "\t\"sort\"\n")
	if rowSecurity != "" {
		fmt.Fprintf(&buf, "\n\tstegotenancy %q\n", rowSecurity)
	}
	fmt.Fprintf(&buf, ")\n\n")
	buf.WriteString(migrationLedger)

	// Initial migration registration.
	fmt.Fprintf(&buf, "func init() {\n")
	fmt.Fprintf(&buf, "\tRegisterSQL(\"001_initial\", %s)\n", tablesConstant)
	if rowSecurity != "" {
		// The row-level security of the tenancy component comes after every table it covers (ST-SUP-RC27-OC03).
		fmt.Fprintln(&buf, `RegisterSQL("100_tenancy_row_security", stegotenancy.RowSecurity)`)
	}
	fmt.Fprintf(&buf, "}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting migrate: %w", err)
	}

	return gen.File{
		Path:    path.Join(ns, "migrate.go"),
		Content: formatted,
	}, nil
}

// migrationCall is one Register call of a function migration in a generated
// file: the offset where its digest argument goes, the digest it holds, and
// the digest of its body.
type migrationCall struct {
	insert int
	digest string
	sum    string
}

// sealMigrations writes the digest of each function migration into its
// Register call. The digest covers the generated source text of the function
// literal and of each package-level function, constant and variable that the
// literal reaches, so an edit to the migration changes its digest. Types are
// not covered: a model migration follows its models by design.
func sealMigrations(ns string, files []gen.File) error {
	calls, err := migrationCalls(ns, files)
	if err != nil {
		return err
	}
	for i, list := range calls {
		content := files[i].Content
		for j := len(list) - 1; j >= 0; j-- {
			if list[j].digest != "" {
				return fmt.Errorf("%s: migration digest is already set", files[i].Path)
			}
			at := list[j].insert
			content = append(content[:at:at], append([]byte(strconv.Quote(list[j].sum)+", "), content[at:]...)...)
		}
		formatted, err := format.Source(content)
		if err != nil {
			return fmt.Errorf("formatting %s: %w", files[i].Path, err)
		}
		files[i].Content = formatted
	}
	// Formatting must not change a sealed body.
	calls, err = migrationCalls(ns, files)
	if err != nil {
		return err
	}
	for i, list := range calls {
		for _, call := range list {
			if call.digest != call.sum {
				return fmt.Errorf("%s: migration digest does not cover its body", files[i].Path)
			}
		}
	}
	return nil
}

// migrationCalls finds the Register calls of function migrations in the Go
// files of the namespace, by file index, and computes the digest of each body.
func migrationCalls(ns string, files []gen.File) (map[int][]migrationCall, error) {
	type declaration struct {
		node ast.Node
		text string
	}
	fset := token.NewFileSet()
	parsed := map[int]*ast.File{}
	declarations := map[string]declaration{}
	for i, file := range files {
		if path.Dir(file.Path) != ns || path.Ext(file.Path) != ".go" {
			continue
		}
		tree, err := parser.ParseFile(fset, file.Path, file.Content, 0)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", file.Path, err)
		}
		parsed[i] = tree
		text := func(node ast.Node) string {
			return string(file.Content[fset.Position(node.Pos()).Offset:fset.Position(node.End()).Offset])
		}
		for _, decl := range tree.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil && decl.Name.Name != "init" {
					declarations[decl.Name.Name] = declaration{decl, text(decl)}
				}
			case *ast.GenDecl:
				if decl.Tok != token.CONST && decl.Tok != token.VAR {
					continue
				}
				for _, spec := range decl.Specs {
					for _, name := range spec.(*ast.ValueSpec).Names {
						declarations[name.Name] = declaration{spec, text(spec)}
					}
				}
			}
		}
	}
	references := func(node ast.Node) []string {
		var names []string
		var visit func(ast.Node) bool
		visit = func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				ast.Inspect(n.X, visit)
				return false
			case *ast.Ident:
				names = append(names, n.Name)
			}
			return true
		}
		ast.Inspect(node, visit)
		return names
	}
	calls := map[int][]migrationCall{}
	indexes := make([]int, 0, len(parsed))
	for i := range parsed {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		content := files[i].Content
		ast.Inspect(parsed[i], func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name, ok := call.Fun.(*ast.Ident); !ok || name.Name != "Register" || len(call.Args) < 2 || len(call.Args) > 3 {
				return true
			}
			literal, ok := call.Args[len(call.Args)-1].(*ast.FuncLit)
			if !ok {
				return true
			}
			var body strings.Builder
			body.Write(content[fset.Position(literal.Pos()).Offset:fset.Position(literal.End()).Offset])
			reached := map[string]bool{}
			queue := references(literal)
			for len(queue) > 0 {
				name := queue[0]
				queue = queue[1:]
				if reached[name] {
					continue
				}
				if found, ok := declarations[name]; ok {
					reached[name] = true
					queue = append(queue, references(found.node)...)
				}
			}
			names := make([]string, 0, len(reached))
			for name := range reached {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				body.WriteString("\n")
				body.WriteString(declarations[name].text)
			}
			sum := sha256.Sum256([]byte(body.String()))
			sealed := migrationCall{insert: fset.Position(literal.Pos()).Offset, sum: hex.EncodeToString(sum[:])}
			if len(call.Args) == 3 {
				if digest, ok := call.Args[1].(*ast.BasicLit); ok && digest.Kind == token.STRING {
					sealed.digest, _ = strconv.Unquote(digest.Value)
				} else {
					sealed.digest = "?"
				}
			}
			calls[i] = append(calls[i], sealed)
			return true
		})
	}
	return calls, nil
}

// tableName converts an entity name to a PostgreSQL table name:
// PascalCase to snake_case, then pluralize using English rules.
func tableName(entityName string) string {
	return pluralize(toSnakeCase(entityName))
}

// pluralize applies basic English pluralization rules to a snake_case word.
func pluralize(s string) string {
	if s == "" {
		return s
	}
	if strings.HasSuffix(s, "s") || strings.HasSuffix(s, "x") || strings.HasSuffix(s, "z") ||
		strings.HasSuffix(s, "sh") || strings.HasSuffix(s, "ch") {
		return s + "es"
	}
	if strings.HasSuffix(s, "y") && len(s) >= 2 {
		preceding := s[len(s)-2]
		if !strings.ContainsRune("aeiou", rune(preceding)) {
			return s[:len(s)-1] + "ies"
		}
	}
	return s + "s"
}

// toSnakeCase converts PascalCase to snake_case, correctly handling
// consecutive uppercase letters (acronyms) like HTTP, API, ID.
func toSnakeCase(s string) string {
	runes := []rune(s)
	var result []rune
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			if unicode.IsLower(prev) {
				result = append(result, '_')
			} else if unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
				result = append(result, '_')
			}
		}
		result = append(result, unicode.ToLower(r))
	}
	return string(result)
}

// toPascalCase converts a snake_case string to PascalCase, treating "id" as
// the acronym "ID" per Go conventions.
func toPascalCase(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if strings.EqualFold(p, "id") {
			parts[i] = "ID"
		} else if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// --- Topological Sort ---

// topoSortEntities topologically sorts entities by ref dependencies so that
// referenced tables are created before referencing tables.
func topoSortEntities(entities []types.Entity) ([]types.Entity, error) {
	entityIndex := make(map[string]int, len(entities))
	for i, e := range entities {
		entityIndex[e.Name] = i
	}

	deps := make(map[string]map[string]bool, len(entities))
	for _, e := range entities {
		d := make(map[string]bool)
		for _, f := range e.Fields {
			if f.Type == types.FieldTypeRef && f.To != "" {
				if _, ok := entityIndex[f.To]; ok && f.To != e.Name {
					d[f.To] = true
				}
			}
		}
		deps[e.Name] = d
	}

	var sorted []types.Entity
	visited := make(map[string]bool, len(entities))

	for len(sorted) < len(entities) {
		var ready []string
		for _, e := range entities {
			if visited[e.Name] {
				continue
			}
			allSatisfied := true
			for dep := range deps[e.Name] {
				if !visited[dep] {
					allSatisfied = false
					break
				}
			}
			if allSatisfied {
				ready = append(ready, e.Name)
			}
		}
		if len(ready) == 0 {
			var cycle []string
			for _, e := range entities {
				if !visited[e.Name] {
					cycle = append(cycle, e.Name)
				}
			}
			return nil, fmt.Errorf("circular ref dependency among entities: %s",
				strings.Join(cycle, " → "))
		}
		for _, name := range ready {
			visited[name] = true
			sorted = append(sorted, entities[entityIndex[name]])
		}
	}

	return sorted, nil
}

// --- Validation ---

func validateEntityUniqueness(entities []types.Entity) error {
	seen := make(map[string]int, len(entities))
	var dupes []string
	for _, e := range entities {
		seen[e.Name]++
		if seen[e.Name] == 2 {
			dupes = append(dupes, e.Name)
		}
	}
	if len(dupes) > 0 {
		return fmt.Errorf("duplicate entity names: %s; each entity must have a unique name",
			strings.Join(dupes, ", "))
	}
	return nil
}

func validateCaseInsensitiveUniqueness(entities []types.Entity) error {
	seen := make(map[string]string, len(entities))
	var errs []string
	for _, e := range entities {
		tbl := tableName(e.Name)
		if existing, ok := seen[tbl]; ok {
			errs = append(errs, fmt.Sprintf(
				"entities %q and %q both produce table name %q",
				existing, e.Name, tbl))
		} else {
			seen[tbl] = e.Name
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("derived table name collisions:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateReservedNames(entities []types.Entity) error {
	for _, e := range entities {
		if reservedTypeNames[e.Name] {
			return fmt.Errorf(
				"entity name %q collides with postgres-adapter generator internal identifier %q; rename the entity to avoid a redeclaration error in generated code",
				e.Name, e.Name)
		}
	}
	return nil
}

func validateRefTargets(entities []types.Entity) error {
	entityNames := make(map[string]bool, len(entities))
	for _, e := range entities {
		entityNames[e.Name] = true
	}

	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			if f.Type == types.FieldTypeRef {
				if f.To == "" {
					errs = append(errs, fmt.Sprintf(
						"entity %s: field %q has type ref but no 'to' attribute — ref fields must specify the target entity",
						e.Name, f.Name))
				} else if !entityNames[f.To] {
					errs = append(errs, fmt.Sprintf(
						"entity %s: field %q has type ref with to: %q, but entity %q does not exist",
						e.Name, f.Name, f.To, f.To))
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("unresolved ref targets:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func validateFieldUniqueness(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		seen := make(map[string]bool, len(e.Fields))
		for _, f := range e.Fields {
			if seen[f.Name] {
				errs = append(errs, fmt.Sprintf(
					"entity %s: duplicate field name %q",
					e.Name, f.Name))
			}
			seen[f.Name] = true
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("field name uniqueness violations:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateFieldNameCharset(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			if !validFieldNamePattern.MatchString(f.Name) {
				errs = append(errs, fmt.Sprintf(
					"entity %s: field name %q contains invalid characters; field names must match %s",
					e.Name, f.Name, validFieldNamePattern.String()))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid field name characters:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateEntityNameCharset(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		if !validFieldNamePattern.MatchString(e.Name) {
			errs = append(errs, fmt.Sprintf(
				"entity name %q contains invalid characters; entity names must match %s",
				e.Name, validFieldNamePattern.String()))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid entity name characters:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateNoImplicitIDCollision(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			if toPascalCase(f.Name) == "ID" {
				errs = append(errs, fmt.Sprintf(
					"entity %s: field %q collides with the implicit primary key column — entities cannot declare a field named %q",
					e.Name, f.Name, f.Name))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("implicit ID collisions:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

var validGoIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func validateDerivedEntityValidity(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		if e.Name == "_" {
			errs = append(errs, fmt.Sprintf(
				"entity name %q is the Go blank identifier and cannot be used as a type name",
				e.Name))
			continue
		}
		if !validGoIdentifier.MatchString(e.Name) {
			errs = append(errs, fmt.Sprintf(
				"entity name %q is not a valid Go identifier",
				e.Name))
			continue
		}
		allUnderscore := true
		for _, r := range e.Name {
			if r != '_' {
				allUnderscore = false
				break
			}
		}
		if allUnderscore {
			errs = append(errs, fmt.Sprintf(
				"entity name %q consists only of underscores and cannot be used as a Go type name",
				e.Name))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid derived entity identifiers:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateDerivedFieldValidity(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			pascal := toPascalCase(f.Name)
			if !validGoIdentifier.MatchString(pascal) {
				errs = append(errs, fmt.Sprintf(
					"entity %s: field %q transforms to %q via toPascalCase, which is not a valid Go identifier",
					e.Name, f.Name, pascal))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid derived field identifiers:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateDerivedFieldUniqueness(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		seen := make(map[string]string, len(e.Fields))
		for _, f := range e.Fields {
			pascal := toPascalCase(f.Name)
			if existing, ok := seen[pascal]; ok && existing != f.Name {
				errs = append(errs, fmt.Sprintf(
					"entity %s: fields %q and %q both produce Go struct field name %q",
					e.Name, existing, f.Name, pascal))
			} else {
				seen[pascal] = f.Name
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("derived field name collisions:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

func validateEnumValues(entities []types.Entity) error {
	var errs []string
	for _, e := range entities {
		for _, f := range e.Fields {
			if f.Type != types.FieldTypeEnum {
				continue
			}
			if len(f.Values) == 0 {
				errs = append(errs, fmt.Sprintf(
					"entity %s: field %q has type enum but no values — enum fields must specify at least one value",
					e.Name, f.Name))
				continue
			}
			seen := make(map[string]bool, len(f.Values))
			for _, v := range f.Values {
				if seen[v] {
					errs = append(errs, fmt.Sprintf(
						"entity %s: field %q has duplicate enum value %q",
						e.Name, f.Name, v))
				}
				seen[v] = true
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid enum fields:\n  %s",
			strings.Join(errs, "\n  "))
	}
	return nil
}

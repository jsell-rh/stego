package manifest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The sync trigger of a schema step (ST-SUP-RP15-LC02): the expand step of a rename, a retype or a require adds a
// column U beside the column K that the release before reads and writes, and a BEFORE row trigger on INSERT OR
// UPDATE keeps the two equal. Its function is a fixed template, so the release before can accept it by its text: the
// generator of the step renders the function with SyncSource, and Check accepts a trigger that the manifest does not
// name only when its function is byte for byte what SyncSource gives for one of its columns, a new column, one
// conversion of SyncConversions and a version of SyncVersions.

// SyncConfig is the one setting of a sync function: its search path, as pg_proc.proconfig holds it. The template has
// no conversion whose result depends on another setting, such as TimeZone or DateStyle (decision on finding F4 (c),
// orchestrator under the owner's delegation, 2026-11-13).
const SyncConfig = "search_path=pg_catalog, pg_temp"

// SchemaStepSetting is the writer marker of the sync trigger: the runtime of the release of a step sets it with SET
// LOCAL in each transaction that writes, to the IDs of the open changes of that release, joined by commas
// (SchemaStepValue). The sync trigger of a change that the marker names copies U into K; else it copies K into U
// (ST-SUP-RP15-LC02 rule 3; decision on finding F3, orchestrator under the owner's delegation, 2026-11-13).
const SchemaStepSetting = "stego.schema_step"

// BackfillSetting is the setting of the backfill batch (ST-SUP-RC01-OC32): the sync trigger of the change that it
// names does nothing in the transaction, when the role that writes is the backfill role, the owner of the manifest.
const BackfillSetting = "stego.backfill_change"

// SchemaStepValue gives the value of SchemaStepSetting for the open changes of a release: their IDs, joined by commas.
func SchemaStepValue(changes []string) (string, error) {
	for _, c := range changes {
		if !changeID.MatchString(c) {
			return "", fmt.Errorf("manifest: %q is not the ID of a schema change", c)
		}
	}
	return strings.Join(changes, ","), nil
}

// changeID is the grammar of the ID of a schema change: lower kebab case.
var changeID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// SyncVersions are the versions of the sync template that this release accepts. A change of the template or of the
// conversion list is a new version, and a release must accept both versions before a step uses the new one.
var SyncVersions = []int{1}

// syncTemplates are the bodies of the template versions: %[1]s is K and %[2]s is U, each as quote_ident gives it,
// %[3]s the conversion of NEW.K to the type of U and %[4]s the conversion of NEW.U to the type of K, %[5]s the change
// ID and %[6]s the backfill role, each as a string literal.
//
// In a transaction where BackfillSetting names the change and the role is the backfill role, the function does
// nothing (ST-SUP-RC01-OC32). Else the writer marker SchemaStepSetting picks the direction: when it names the change,
// the writer is the release of the step, which writes only U, and the function sets K from U; when it does not, the
// writer is the release before, which writes only K, and the function sets U from K, by a conversion that never
// raises (ST-SUP-RC01-OC33). On UPDATE it sets the other column only when one of the two changed. It refuses a write
// that sets both columns to values that differ after the conversion, with a fixed message that holds no row value:
// an UPDATE that changes both, and an INSERT of the release before that sets U, which has no default
// (ST-SUP-RC01-OC23, ST-SUP-RC01-OC13; decisions on ST-GEN-Q17 and on finding F3).
var syncTemplates = map[int]string{1: `
BEGIN
  IF pg_catalog.current_setting('stego.backfill_change', true) = %[5]s AND CURRENT_USER = %[6]s THEN
    RETURN NEW;
  END IF;
  IF pg_catalog.strpos(',' || pg_catalog.current_setting('stego.schema_step', true) || ',', ',' || %[5]s || ',') > 0 THEN
    IF TG_OP = 'INSERT' THEN
      NEW.%[1]s := %[4]s;
    ELSIF NEW.%[1]s IS DISTINCT FROM OLD.%[1]s OR NEW.%[2]s IS DISTINCT FROM OLD.%[2]s THEN
      IF NEW.%[1]s IS DISTINCT FROM OLD.%[1]s AND NEW.%[2]s IS DISTINCT FROM OLD.%[2]s AND NEW.%[1]s IS DISTINCT FROM %[4]s THEN
        RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'stego sync trigger: the write sets the old and the new column to values that differ';
      END IF;
      NEW.%[1]s := %[4]s;
    END IF;
  ELSIF TG_OP = 'INSERT' THEN
    IF NEW.%[2]s IS NOT NULL AND NEW.%[2]s IS DISTINCT FROM %[3]s THEN
      RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'stego sync trigger: the write sets the old and the new column to values that differ';
    END IF;
    NEW.%[2]s := %[3]s;
  ELSIF NEW.%[1]s IS DISTINCT FROM OLD.%[1]s OR NEW.%[2]s IS DISTINCT FROM OLD.%[2]s THEN
    IF NEW.%[1]s IS DISTINCT FROM OLD.%[1]s AND NEW.%[2]s IS DISTINCT FROM OLD.%[2]s AND NEW.%[2]s IS DISTINCT FROM %[3]s THEN
      RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'stego sync trigger: the write sets the old and the new column to values that differ';
    END IF;
    NEW.%[2]s := %[3]s;
  END IF;
  RETURN NEW;
END;
`}

// Conversion is one conversion of the fixed list: its name, the types of K and U as format_type gives them (both
// empty for the identity, which needs the same type), and the expressions of each direction, each a pg_catalog
// cast of its one argument %s. Guard, for a conversion that can fail, is the condition, of its one argument %s, under
// which Forward converts; where it does not hold, the forward conversion gives the fill value of the change, or NULL,
// and never raises, so no write of the release before fails (ST-SUP-RC01-OC33). Back is the inverse, which
// ST-DECL-INV63 makes total.
type Conversion struct {
	Name, From, To, Forward, Back, Guard string
}

// SyncConversions are the conversions of the sync template, the same for each version of SyncVersions. No
// conversion reads a setting of the session: the conversions of a time to text and back, whose text depends on
// TimeZone and DateStyle, are not in the list (decision on finding F4 (c)).
var SyncConversions = func() []Conversion {
	list := []Conversion{
		{Name: "identity", Forward: "%s", Back: "%s"},
		{Name: "integer-bigint", From: "integer", To: "bigint", Forward: "CAST(%s AS pg_catalog.int8)", Back: "CAST(%s AS pg_catalog.int4)"},
		{Name: "bigint-integer", From: "bigint", To: "integer", Forward: "CAST(%s AS pg_catalog.int4)", Back: "CAST(%s AS pg_catalog.int8)",
			Guard: "%s BETWEEN -2147483648 AND 2147483647"},
	}
	for _, t := range []struct{ format, catalog string }{
		{"integer", "int4"}, {"bigint", "int8"}, {"numeric", "numeric"}, {"boolean", "bool"}, {"uuid", "uuid"},
	} {
		list = append(list,
			Conversion{Name: t.catalog + "-text", From: t.format, To: "text", Forward: "CAST(%s AS pg_catalog.text)", Back: "CAST(%s AS pg_catalog." + t.catalog + ")"},
			Conversion{Name: "text-" + t.catalog, From: "text", To: t.format, Forward: "CAST(%s AS pg_catalog." + t.catalog + ")", Back: "CAST(%s AS pg_catalog.text)",
				Guard: "pg_catalog.pg_input_is_valid(%s, 'pg_catalog." + t.catalog + "')"})
	}
	return list
}()

// SyncConversion gives the conversion of the list by its name.
func SyncConversion(name string) (Conversion, bool) {
	i := slices.IndexFunc(SyncConversions, func(c Conversion) bool { return c.Name == name })
	if i < 0 {
		return Conversion{}, false
	}
	return SyncConversions[i], true
}

// fits tells whether the conversion takes K of the type from to U of the type to.
func (c Conversion) fits(from, to string) bool {
	if c.From == "" && c.To == "" {
		return from == to
	}
	return c.From == from && c.To == to
}

// Sync is what a sync function is rendered from: the change ID, the backfill role (the owner of the manifest), the
// columns K and U, the conversion, and, for a conversion with a guard, the fill value of the change as the text of
// a constant of the type of U, or no fill value (NULL).
type Sync struct {
	Change, Owner, K, U string
	Conversion          Conversion
	Fill                string
	HasFill             bool
}

// syncLiteral gives the text as a SQL string literal. It refuses a backslash, which a session with
// standard_conforming_strings off reads as an escape, a control character, and a dollar sign, which could end the
// dollar quote of the function body (syncQuote).
func syncLiteral(text string) (string, error) {
	if strings.ContainsAny(text, "\\$") || strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 }) {
		return "", fmt.Errorf("manifest: the sync function takes no backslash, dollar sign or control character in a constant")
	}
	return "'" + strings.ReplaceAll(text, "'", "''") + "'", nil
}

// SyncSource gives the source of the sync function of version for s: the text between the dollar quotes of its
// CREATE FUNCTION, which pg_proc.prosrc holds.
func SyncSource(version int, s Sync) (string, error) {
	template, ok := syncTemplates[version]
	if !ok || !slices.Contains(SyncVersions, version) {
		return "", fmt.Errorf("manifest: the sync template has no version %d", version)
	}
	if !name.MatchString(s.K) || !name.MatchString(s.U) || s.K == s.U {
		return "", fmt.Errorf("manifest: the sync function needs two different column names of at most 63 bytes")
	}
	if strings.Contains(s.K, "$") || strings.Contains(s.U, "$") {
		return "", fmt.Errorf("manifest: the sync function takes no dollar sign in a column name")
	}
	if !changeID.MatchString(s.Change) || !name.MatchString(s.Owner) {
		return "", fmt.Errorf("manifest: the sync function needs the ID of its change and the backfill role")
	}
	c, ok := SyncConversion(s.Conversion.Name)
	if !ok {
		return "", fmt.Errorf("manifest: the conversion %q is not in the list", s.Conversion.Name)
	}
	if s.HasFill && c.Guard == "" {
		return "", fmt.Errorf("manifest: the conversion %q cannot fail and takes no fill value", c.Name)
	}
	qk, qu := QuoteIdent(s.K), QuoteIdent(s.U)
	forward := fmt.Sprintf(c.Forward, "NEW."+qk)
	if c.Guard != "" {
		fill := "NULL"
		if s.HasFill {
			text, err := syncLiteral(s.Fill)
			if err != nil {
				return "", err
			}
			fill = text
		}
		// In parentheses, as PL/pgSQL ends the condition of an IF at the first THEN outside them.
		forward = "(CASE WHEN NEW." + qk + " IS NULL THEN NULL WHEN " + fmt.Sprintf(c.Guard, "NEW."+qk) + " THEN " + forward + " ELSE " + fill + " END)"
	}
	change, _ := syncLiteral(s.Change)
	owner, err := syncLiteral(s.Owner)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(template, qk, qu, forward, fmt.Sprintf(c.Back, "NEW."+qu), change, owner), nil
}

// SyncFunction gives the CREATE FUNCTION statement of a sync function named schema.function, owned by the role that
// runs it: plpgsql, SECURITY INVOKER, with only the setting SyncConfig, returning trigger and volatile.
func SyncFunction(schema, function string, version int, s Sync) (string, error) {
	source, err := SyncSource(version, s)
	if err != nil {
		return "", err
	}
	if !name.MatchString(schema) || !name.MatchString(function) {
		return "", fmt.Errorf("manifest: the sync function needs a schema and a name of at most 63 bytes")
	}
	// The body is in the dollar quote syncQuote, so a source that holds it would end the quote, and the text after it
	// would run as SQL as the role of the migration.
	if strings.Contains(source, syncQuote) {
		return "", fmt.Errorf("manifest: the source of the sync function holds its dollar quote %s", syncQuote)
	}
	return "CREATE FUNCTION " + QuoteIdent(schema) + "." + QuoteIdent(function) + "() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER VOLATILE SET search_path = pg_catalog, pg_temp AS " + syncQuote +
		source + syncQuote, nil
}

// syncQuote is the dollar quote of the body of a sync function in its CREATE FUNCTION.
const syncQuote = "$stego_sync$"

// plainIdent is a name that quote_ident leaves as it is when it is no keyword that is not unreserved.
var plainIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// reservedWords are the PostgreSQL 17 keywords that are not unreserved, from pg_get_keywords(), which quote_ident
// quotes. Check compares QuoteIdent with quote_ident of the database for each column it accepts, so a list that
// falls behind the server refuses, and never accepts, a function.
var reservedWords = map[string]bool{}

func init() {
	for _, word := range strings.Fields(`all analyse analyze and any array as asc asymmetric authorization between bigint
binary bit boolean both case cast char character check coalesce collate collation column concurrently constraint
create cross current_catalog current_date current_role current_schema current_time current_timestamp current_user
dec decimal default deferrable desc distinct do else end except exists extract false fetch float for foreign freeze
from full grant greatest group grouping having ilike in initially inner inout int integer intersect interval into
is isnull join json json_array json_arrayagg json_exists json_object json_objectagg json_query json_scalar
json_serialize json_table json_value lateral leading least left like limit localtime localtimestamp merge_action
national natural nchar none normalize not notnull null nullif numeric offset on only or order out outer overlaps
overlay placing position precision primary real references returning right row select session_user setof similar
smallint some substring symmetric system_user table tablesample then time timestamp to trailing treat trim true
union unique user using values varchar variadic verbose when where window with xmlattributes xmlconcat xmlelement
xmlexists xmlforest xmlnamespaces xmlparse xmlpi xmlroot xmlserialize xmltable`) {
		reservedWords[word] = true
	}
}

// QuoteIdent gives a name as quote_ident of PostgreSQL 17 gives it: as it is when it is a lower-case name that is no
// keyword that is not unreserved, and else in double quotes with each double quote doubled.
func QuoteIdent(n string) string {
	if plainIdent.MatchString(n) && !reservedWords[n] {
		return n
	}
	return quoteIdent(n)
}

// ForeignKeyName gives the name that PostgreSQL and the generator give the foreign key of the column of a table:
// <table>_<column>_fkey, or, when that is longer than 63 bytes, its first 46 bytes, an underscore and the first 16
// hexadecimal digits of its SHA-256 digest, as the generator's sqlName gives it (ST-SUP-RP15-LC03).
func ForeignKeyName(table, column string) string {
	n := table + "_" + column + "_fkey"
	if len(n) <= 63 {
		return n
	}
	return n[:46] + "_" + digestPrefix(n)
}

// digestPrefix gives the first 16 hexadecimal digits of the SHA-256 digest of n.
func digestPrefix(n string) string {
	sum := sha256.Sum256([]byte(n))
	return hex.EncodeToString(sum[:8])
}

// stepState is what Check accepts of the schema steps across the tables: the held schema of the sync functions,
// the owner of the manifest, the oids of the tables of the manifest that are tables of declared entities, of those
// that are not owned (no row-level security in the manifest), and the oids of the sync functions that an accepted
// sync trigger runs, which the Steps schema may hold.
type stepState struct {
	schema, owner    string
	entities, shared map[uint32]bool
	accepted         map[uint32]bool
}

// builtinTypeSQL tells, for the row of pg_catalog.pg_type ty, that the type is a base type that initdb made in
// pg_catalog: its input, output and comparison functions are built in, and no domain CHECK, enum or range runs.
const builtinTypeSQL = `(ty.oid < 16384 AND ty.typtype = 'b' AND ty.typnamespace = 'pg_catalog'::pg_catalog.regnamespace)`

// tableColumn is one column of a table as the catalog holds it, for the step rules. builtin tells that its type is a
// built-in base type of pg_catalog (builtinTypeSQL).
type tableColumn struct {
	typ                                   string
	notNull, hasDefault, derived, builtin bool
}

// stepColumns reads the columns of the table rel, and the names of the columns of its primary key and of each of its
// unique indexes.
func stepColumns(ctx context.Context, tx *sql.Tx, rel uint32) (map[string]tableColumn, map[string]bool, error) {
	columns := map[string]tableColumn{}
	rows, err := tx.QueryContext(ctx, `SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull, a.atthasdef,
  a.attgenerated <> '' OR a.attidentity <> '', `+builtinTypeSQL+`
  FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type ty ON ty.oid = a.atttypid
  WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped`, rel)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var n string
		var c tableColumn
		if err := rows.Scan(&n, &c.typ, &c.notNull, &c.hasDefault, &c.derived, &c.builtin); err != nil {
			rows.Close()
			return nil, nil, err
		}
		columns[n] = c
	}
	if err := closed(rows); err != nil {
		return nil, nil, err
	}
	keyed := map[string]bool{}
	rows, err = tx.QueryContext(ctx, `SELECT COALESCE(a.attname, '') FROM pg_catalog.pg_index i
  CROSS JOIN LATERAL pg_catalog.unnest(i.indkey::pg_catalog.int2[]) AS k(n)
  LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.n
  WHERE i.indrelid = $1 AND (i.indisunique OR i.indisprimary)`, rel)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, nil, err
		}
		keyed[n] = true
	}
	return columns, keyed, closed(rows)
}

// fixedColumns are the columns that the runtime library writes on each entity table, which a sync trigger never
// moves (ST-SUP-RP15-LC02 rule 2).
var fixedColumns = map[string]bool{"id": true, "version": true, "authz_token": true, "created_time": true, "updated_time": true}

// The constants of a sync function that its source names, which syncPair reads back to render it again: the change
// ID, and the fill value or NULL.
var (
	syncChange = regexp.MustCompile(`current_setting\('stego\.backfill_change', true\) = '([a-z0-9-]+)' AND`)
	syncFill   = regexp.MustCompile(` ELSE ('(?:[^']|'')*'|NULL) END\)`)
)

// syncPair finds the columns K and U, the version and the conversion for which SyncSource gives source with the
// backfill role owner, among the pairs that pairs gives; ok is false when none does.
func syncPair(source, owner string, pairs [][2]string, columns map[string]tableColumn) (k, u string, version int, c Conversion, ok bool) {
	change := syncChange.FindStringSubmatch(source)
	if change == nil {
		return "", "", 0, Conversion{}, false
	}
	s := Sync{Change: change[1], Owner: owner}
	if fill := syncFill.FindStringSubmatch(source); fill != nil && fill[1] != "NULL" {
		s.Fill, s.HasFill = strings.ReplaceAll(fill[1][1:len(fill[1])-1], "''", "'"), true
	}
	for _, pair := range pairs {
		for _, v := range SyncVersions {
			for _, conv := range SyncConversions {
				if !conv.fits(columns[pair[0]].typ, columns[pair[1]].typ) {
					continue
				}
				s.K, s.U, s.Conversion = pair[0], pair[1], conv
				if s.HasFill && conv.Guard == "" {
					continue
				}
				if text, err := SyncSource(v, s); err == nil && text == source {
					return pair[0], pair[1], v, conv, true
				}
			}
		}
	}
	return "", "", 0, Conversion{}, false
}

// syncTrigger is a trigger that the manifest does not name, as the step rules read it.
type syncTrigger struct {
	tgtype                                   int16
	enabled, attrs, language, source, config string
	owner, schema                            string
	function                                 uint32
	parent                                   uint32
	plain, definer, fits, strict, retset     bool
	bound                                    int
}

// acceptSync tells whether the trigger n of the entity table t (rel) is the sync trigger of a later step in its strict
// template form (ST-SUP-RP15-LC02 rules 2 to 6), and gives the oid of its function and its columns K and U. Rule 1,
// the scope, and rule 7 are the caller's. used are the columns that another sync trigger of the table moves.
func acceptSync(ctx context.Context, tx *sql.Tx, t Table, rel uint32, n string, steps *stepState, columns map[string]tableColumn,
	keyed, used map[string]bool) (uint32, [2]string, bool, error) {
	var tr syncTrigger
	err := tx.QueryRowContext(ctx, `SELECT t.tgtype, t.tgenabled::text, t.tgparentid,
  t.tgqual IS NULL AND t.tgnargs = 0,
  COALESCE((SELECT pg_catalog.string_agg(a.attname, chr(31) ORDER BY a.attname) FROM pg_catalog.unnest(t.tgattr::pg_catalog.int2[]) AS k(n)
    JOIN pg_catalog.pg_attribute a ON a.attrelid = t.tgrelid AND a.attnum = k.n), ''),
  p.oid, l.lanname, p.prosrc, COALESCE(pg_catalog.array_to_string(p.proconfig, chr(31)), ''), p.prosecdef,
  p.prorettype = 'pg_catalog.trigger'::pg_catalog.regtype AND p.pronargs = 0 AND p.provolatile = 'v' AND p.prokind = 'f',
  p.proisstrict, p.proretset, pg_catalog.pg_get_userbyid(p.proowner), pn.nspname,
  (SELECT pg_catalog.count(*) FROM pg_catalog.pg_trigger x WHERE x.tgfoid = p.oid)
  FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_proc p ON p.oid = t.tgfoid
  JOIN pg_catalog.pg_namespace pn ON pn.oid = p.pronamespace JOIN pg_catalog.pg_language l ON l.oid = p.prolang
  WHERE t.tgrelid = $1 AND t.tgname = $2 AND NOT t.tgisinternal`, rel, n).Scan(&tr.tgtype, &tr.enabled, &tr.parent, &tr.plain,
		&tr.attrs, &tr.function, &tr.language, &tr.source, &tr.config, &tr.definer, &tr.fits, &tr.strict, &tr.retset, &tr.owner,
		&tr.schema, &tr.bound)
	if err != nil {
		return 0, [2]string{}, false, err
	}
	// Rule 5: BEFORE, FOR EACH ROW, on INSERT OR UPDATE only, no WHEN condition, no argument, enabled, no clone.
	if tr.tgtype != triggerRow|triggerBefore|triggerInsert|triggerUpdate || !tr.plain || tr.parent != 0 ||
		(tr.enabled != "O" && tr.enabled != "A") {
		return 0, [2]string{}, false, nil
	}
	// Rule 4: plpgsql, SECURITY INVOKER, only the setting SyncConfig, returns trigger with no argument, volatile, not
	// strict, owned by the owner of the manifest, in the held schema of the sync functions, bound to this trigger only.
	if steps.schema == "" || steps.owner == "" || tr.language != "plpgsql" || tr.definer || tr.config != SyncConfig || !tr.fits ||
		tr.strict || tr.retset || tr.owner != steps.owner || tr.schema != steps.schema || tr.bound != 1 {
		return 0, [2]string{}, false, nil
	}
	// Rule 2: the pair. K is a column of the manifest and U a column that it does not list.
	listed := map[string]bool{}
	for _, c := range t.Columns {
		listed[c.Name] = true
	}
	var pairs [][2]string
	if tr.attrs != "" {
		attrs := strings.Split(tr.attrs, "\x1f")
		if len(attrs) != 2 {
			return 0, [2]string{}, false, nil
		}
		pairs = [][2]string{{attrs[0], attrs[1]}, {attrs[1], attrs[0]}}
	} else {
		for k := range listed {
			for u := range columns {
				if !listed[u] {
					pairs = append(pairs, [2]string{k, u})
				}
			}
		}
		slices.SortFunc(pairs, func(a, b [2]string) int { return strings.Compare(a[0]+"\x00"+a[1], b[0]+"\x00"+b[1]) })
	}
	var candidates [][2]string
	for _, pair := range pairs {
		k, u := pair[0], pair[1]
		kc, kok := columns[k]
		uc, uok := columns[u]
		if !kok || !uok || !listed[k] || listed[u] || keyed[k] || fixedColumns[k] || used[k] || used[u] || uc.derived || uc.hasDefault || kc.derived {
			continue
		}
		candidates = append(candidates, pair)
	}
	// Rules 3, 4 and 6: the body is the template of a version of this release, for the pair and one conversion.
	k, u, _, _, ok := syncPair(tr.source, steps.owner, candidates, columns)
	if !ok {
		return 0, [2]string{}, false, nil
	}
	// QuoteIdent must be the quote_ident of the server, so a keyword list that falls behind refuses the trigger.
	var qk, qu string
	if err := tx.QueryRowContext(ctx, `SELECT pg_catalog.quote_ident($1), pg_catalog.quote_ident($2)`, k, u).Scan(&qk, &qu); err != nil {
		return 0, [2]string{}, false, err
	}
	if qk != QuoteIdent(k) || qu != QuoteIdent(u) {
		return 0, [2]string{}, false, nil
	}
	return tr.function, [2]string{k, u}, true, nil
}

// acceptForeignKey tells whether the foreign key n of the entity table t (rel) is the foreign key of a later add step
// in its exact form (ST-SUP-RP15-LC03): on one column of the table that is not in the manifest, is nullable and is of
// a built-in base type of pg_catalog; referring to the whole primary key of the table of a declared entity of the
// manifest, which ends with the key id; with the tenant column of the table first when the referenced table is owned;
// on one column only when the referenced table is a table of a declared entity that is not owned, whose primary key
// is its key id alone and whose rows each tenant may read, so the check of the key, which runs past row-level
// security, tells a tenant nothing it cannot read; NO ACTION on delete and on update, match simple and not deferrable,
// validated or not yet validated, as the add step leaves it before the migration that validates it (decision on
// finding F2, orchestrator under the owner's delegation, 2026-11-13); and named as ForeignKeyName names it.
func acceptForeignKey(ctx context.Context, tx *sql.Tx, t Table, rel uint32, n string, steps *stepState, columns map[string]tableColumn) (bool, error) {
	var from, to, ownKey, refKey, actions string
	var target uint32
	var deferrable, targetRows bool
	keyOf := `COALESCE((SELECT pg_catalog.string_agg(a.attname, chr(31) ORDER BY k.o) FROM pg_catalog.pg_constraint pk
    CROSS JOIN LATERAL pg_catalog.unnest(pk.conkey) WITH ORDINALITY AS k(n, o)
    JOIN pg_catalog.pg_attribute a ON a.attrelid = pk.conrelid AND a.attnum = k.n WHERE pk.conrelid = %s AND pk.contype = 'p'), '')`
	err := tx.QueryRowContext(ctx, `SELECT
  COALESCE((SELECT pg_catalog.string_agg(a.attname, chr(31) ORDER BY k.o) FROM pg_catalog.unnest(c.conkey) WITH ORDINALITY AS k(n, o)
    JOIN pg_catalog.pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.n), ''),
  COALESCE((SELECT pg_catalog.string_agg(a.attname, chr(31) ORDER BY k.o) FROM pg_catalog.unnest(c.confkey) WITH ORDINALITY AS k(n, o)
    JOIN pg_catalog.pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = k.n), ''),
  `+fmt.Sprintf(keyOf, "c.conrelid")+`, `+fmt.Sprintf(keyOf, "c.confrelid")+`,
  c.confrelid, c.confdeltype::text || c.confupdtype::text || c.confmatchtype::text, c.condeferrable OR c.condeferred,
  (SELECT r.relrowsecurity FROM pg_catalog.pg_class r WHERE r.oid = c.confrelid)
  FROM pg_catalog.pg_constraint c WHERE c.conrelid = $1 AND c.conname = $2 AND c.contype = 'f'`, rel, n).Scan(
		&from, &to, &ownKey, &refKey, &target, &actions, &deferrable, &targetRows)
	if err != nil {
		return false, err
	}
	fromCols, refCols, ownCols := strings.Split(from, "\x1f"), strings.Split(refKey, "\x1f"), strings.Split(ownKey, "\x1f")
	column := fromCols[len(fromCols)-1]
	listed := slices.ContainsFunc(t.Columns, func(c Column) bool { return c.Name == column })
	c, ok := columns[column]
	switch {
	case !ok || listed || c.notNull || !c.builtin || !steps.entities[target] || actions != "aas" || deferrable:
		return false, nil
	case to != refKey || refCols[len(refCols)-1] != "id" || n != ForeignKeyName(t.Name, column):
		return false, nil
	}
	switch len(refCols) {
	case 1:
		// The referenced key is the key id alone: any table may refer to it with one column when it is the table of a
		// declared entity that is not owned and has no row-level security, so each tenant may read its rows. A runtime
		// table or a table with row-level security keyed by id alone is no target (review of PR #270, M1).
		return len(fromCols) == 1 && steps.shared[target] && !targetRows, nil
	case 2:
		// The referenced key holds the tenant column: the table is owned, and its tenant column, the first column of
		// its primary key, comes first.
		return len(ownCols) == 2 && ownCols[1] == "id" && len(fromCols) == 2 && fromCols[0] == ownCols[0] && ownCols[0] == refCols[0], nil
	}
	return false, nil
}

// CheckName gives the name that PostgreSQL and the generator give the CHECK of the column of a table:
// <table>_<column>_check, or, when that is longer than 63 bytes, its first 46 bytes, an underscore and the first 16
// hexadecimal digits of its SHA-256 digest, as the generator's sqlName gives it.
func CheckName(table, column string) string {
	n := table + "_" + column + "_check"
	if len(n) <= 63 {
		return n
	}
	return n[:46] + "_" + digestPrefix(n)
}

// acceptUnique tells whether the unique index n of the entity table t, with the key columns key, is the unique index of
// a later add step in its exact form: the columns of the primary key of the table (primary) but its last, the key id,
// first, which on an owned table is the tenant column, so the key is unique within one tenant; then one or more
// columns, each of a built-in base type of pg_catalog, of which at least one is a column that the manifest does not
// list; and named as UniqueName names it. The caller checks that it is no primary key, has no expression or
// predicate, is not deferrable, treats nulls as distinct and has the default operator classes and collations.
func acceptUnique(t Table, n string, key, primary []string, columns map[string]tableColumn) bool {
	if len(primary) == 0 || primary[len(primary)-1] != "id" {
		return false
	}
	lead := primary[:len(primary)-1]
	if len(key) <= len(lead) || !slices.Equal(key[:len(lead)], lead) {
		return false
	}
	rest := key[len(lead):]
	unlisted := false
	for _, k := range rest {
		c, ok := columns[k]
		if !ok || !c.builtin || slices.Contains(lead, k) {
			return false
		}
		if !slices.ContainsFunc(t.Columns, func(c Column) bool { return c.Name == k }) {
			unlisted = true
		}
	}
	return unlisted && n == UniqueName(t.Name, rest)
}

// UniqueName gives the name that the generator gives the unique index of the key columns of a table, after its tenant
// column: <table>_<column>_..._key, or, when that is longer than 63 bytes, its first 46 bytes, an underscore and the
// first 16 hexadecimal digits of its SHA-256 digest, as the generator's sqlName gives it.
func UniqueName(table string, columns []string) string {
	n := table + "_" + strings.Join(columns, "_") + "_key"
	if len(n) <= 63 {
		return n
	}
	return n[:46] + "_" + digestPrefix(n)
}

// checkNodes are the nodes of the expression of a CHECK that an add step gives: a length, a bound, the finite value of
// a float and the declared values of an enum, joined by AND. Each other node of a stored expression is refused.
var checkNodes = map[string]bool{"BOOLEXPR": true, "OPEXPR": true, "FUNCEXPR": true, "SCALARARRAYOPEXPR": true, "VAR": true,
	"CONST": true, "ARRAYEXPR": true, "NULLTEST": true, "RELABELTYPE": true}

// The parts of the text of a stored expression (pg_node_tree): the tag of each node, and each field with a number.
var (
	nodeTag   = regexp.MustCompile(`\{([A-Z_]+)`)
	nodeField = regexp.MustCompile(`:([a-z_]+) (-?[0-9]+)\b`)
)

// acceptCheck tells whether the CHECK n of the entity table t (rel) is the CHECK of a later add step in its exact form
// (decision on ST-SUP-RP15-LC01 (a)): on one column of the table that is not in the manifest and is of a built-in base
// type of pg_catalog; local and inheritable, validated or not yet validated, as the add step leaves it before the
// migration that validates it (decision on finding F2); named as CheckName names it; with an expression that reads
// that column only, holds only the nodes of checkNodes, and whose functions, operators, types and collations are each
// built in (oid below 16384), each function and the function of each operator IMMUTABLE and no set function; and with
// no dependency in pg_depend on an object that is not built in, or on another column or relation. PostgreSQL records
// no dependency on a pinned object, so the expression itself is read: a built-in function that is not IMMUTABLE, such
// as random or set_config, is refused, as is each function of a schema of the database.
func acceptCheck(ctx context.Context, tx *sql.Tx, t Table, rel uint32, n string, columns map[string]tableColumn) (bool, error) {
	var con uint32
	var keys, attnum int
	var column, tree string
	var noInherit, local bool
	err := tx.QueryRowContext(ctx, `SELECT c.oid, COALESCE(pg_catalog.array_length(c.conkey, 1), 0), COALESCE(c.conkey[1], 0),
  COALESCE(a.attname, ''), COALESCE(c.conbin::pg_catalog.text, ''), c.connoinherit, c.conislocal AND c.coninhcount = 0
  FROM pg_catalog.pg_constraint c LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
  WHERE c.conrelid = $1 AND c.conname = $2 AND c.contype = 'c'`, rel, n).Scan(&con, &keys, &attnum, &column, &tree, &noInherit, &local)
	if err != nil {
		return false, err
	}
	listed := slices.ContainsFunc(t.Columns, func(c Column) bool { return c.Name == column })
	c, ok := columns[column]
	if keys != 1 || attnum <= 0 || !ok || listed || !c.builtin || noInherit || !local || n != CheckName(t.Name, column) {
		return false, nil
	}
	// The expression: only the nodes of the step form, reading the one column of conkey.
	for _, m := range nodeTag.FindAllStringSubmatch(tree, -1) {
		if !checkNodes[m[1]] {
			return false, nil
		}
	}
	var functions, operators, others []string
	for _, m := range nodeField.FindAllStringSubmatch(tree, -1) {
		field, value := m[1], m[2]
		switch {
		case field == "varattno" && value != fmt.Sprint(attnum), field == "varlevelsup" && value != "0", field == "varno" && value != "1":
			return false, nil
		case field == "funcid" || field == "opfuncid" || field == "hashfuncid" || field == "negfuncid":
			if value != "0" {
				functions = append(functions, value)
			}
		case field == "opno":
			operators = append(operators, value)
		case strings.HasSuffix(field, "type") || strings.HasSuffix(field, "typeid") || strings.HasSuffix(field, "collid"):
			others = append(others, value)
		}
	}
	if !strings.Contains(tree, "{VAR ") {
		return false, nil
	}
	oids := func(values []string) string { return "{" + strings.Join(values, ",") + "}" }
	var faults int
	err = tx.QueryRowContext(ctx, `SELECT
  (SELECT pg_catalog.count(*) FROM pg_catalog.unnest($1::pg_catalog.oid[]) AS f(oid) LEFT JOIN pg_catalog.pg_proc p ON p.oid = f.oid
    WHERE p.oid IS NULL OR p.oid >= 16384 OR p.provolatile <> 'i' OR p.prokind <> 'f' OR p.proretset)
  + (SELECT pg_catalog.count(*) FROM pg_catalog.unnest($2::pg_catalog.oid[]) AS o(oid) LEFT JOIN pg_catalog.pg_operator op ON op.oid = o.oid
    LEFT JOIN pg_catalog.pg_proc p ON p.oid = op.oprcode
    WHERE op.oid IS NULL OR op.oid >= 16384 OR p.oid IS NULL OR p.oid >= 16384 OR p.provolatile <> 'i' OR p.proretset)
  + (SELECT pg_catalog.count(*) FROM pg_catalog.unnest($3::pg_catalog.oid[]) AS x(oid) WHERE x.oid >= 16384)
  + (SELECT pg_catalog.count(*) FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_catalog.pg_constraint'::pg_catalog.regclass AND d.objid = $4
    AND NOT (d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.refobjid = $5 AND d.refobjsubid = $6)
    AND (d.refobjid >= 16384 OR d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass))`,
		oids(functions), oids(operators), oids(others), con, rel, attnum).Scan(&faults)
	if err != nil {
		return false, err
	}
	return faults == 0, nil
}

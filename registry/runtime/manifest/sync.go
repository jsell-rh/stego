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

// SyncConfig is the one setting of a sync function: its search path, as pg_proc.proconfig holds it.
const SyncConfig = "search_path=pg_catalog, pg_temp"

// SyncVersions are the versions of the sync template that this release accepts. A change of the template or of the
// conversion list is a new version, and a release must accept both versions before a step uses the new one.
var SyncVersions = []int{1}

// syncTemplates are the bodies of the template versions: %[1]s is K and %[2]s is U, each as quote_ident gives it,
// %[3]s the conversion of NEW.K to the type of U and %[4]s the conversion of NEW.U to the type of K. On INSERT the
// function only sets U from K; on UPDATE it copies U into K only when U changed and K did not, and else copies K
// into U.
var syncTemplates = map[int]string{1: `
BEGIN
  IF TG_OP = 'INSERT' THEN
    NEW.%[2]s := %[3]s;
  ELSIF NEW.%[2]s IS DISTINCT FROM OLD.%[2]s AND NEW.%[1]s IS NOT DISTINCT FROM OLD.%[1]s THEN
    NEW.%[1]s := %[4]s;
  ELSE
    NEW.%[2]s := %[3]s;
  END IF;
  RETURN NEW;
END;
`}

// Conversion is one conversion of the fixed list: its name, the types of K and U as format_type gives them (both
// empty for the identity, which needs the same type), and the expressions of each direction, each a pg_catalog
// cast of its one argument %s.
type Conversion struct {
	Name, From, To, Forward, Back string
}

// SyncConversions are the conversions of the sync template, the same for each version of SyncVersions.
var SyncConversions = func() []Conversion {
	list := []Conversion{
		{Name: "identity", Forward: "%s", Back: "%s"},
		{Name: "integer-bigint", From: "integer", To: "bigint", Forward: "CAST(%s AS pg_catalog.int8)", Back: "CAST(%s AS pg_catalog.int4)"},
		{Name: "bigint-integer", From: "bigint", To: "integer", Forward: "CAST(%s AS pg_catalog.int4)", Back: "CAST(%s AS pg_catalog.int8)"},
	}
	for _, t := range []struct{ format, catalog string }{
		{"integer", "int4"}, {"bigint", "int8"}, {"numeric", "numeric"}, {"boolean", "bool"}, {"uuid", "uuid"},
		{"timestamp with time zone", "timestamptz"},
	} {
		list = append(list,
			Conversion{Name: t.catalog + "-text", From: t.format, To: "text", Forward: "CAST(%s AS pg_catalog.text)", Back: "CAST(%s AS pg_catalog." + t.catalog + ")"},
			Conversion{Name: "text-" + t.catalog, From: "text", To: t.format, Forward: "CAST(%s AS pg_catalog." + t.catalog + ")", Back: "CAST(%s AS pg_catalog.text)"})
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

// SyncSource gives the source of the sync function of version for the columns k and u and the conversion c: the
// text between the dollar quotes of its CREATE FUNCTION, which pg_proc.prosrc holds.
func SyncSource(version int, k, u string, c Conversion) (string, error) {
	template, ok := syncTemplates[version]
	if !ok || !slices.Contains(SyncVersions, version) {
		return "", fmt.Errorf("manifest: the sync template has no version %d", version)
	}
	if !name.MatchString(k) || !name.MatchString(u) || k == u {
		return "", fmt.Errorf("manifest: the sync function needs two different column names of at most 63 bytes")
	}
	if _, ok := SyncConversion(c.Name); !ok {
		return "", fmt.Errorf("manifest: the conversion %q is not in the list", c.Name)
	}
	qk, qu := QuoteIdent(k), QuoteIdent(u)
	return fmt.Sprintf(template, qk, qu, fmt.Sprintf(c.Forward, "NEW."+qk), fmt.Sprintf(c.Back, "NEW."+qu)), nil
}

// SyncFunction gives the CREATE FUNCTION statement of a sync function named schema.function, owned by the role that
// runs it: plpgsql, SECURITY INVOKER, with only the setting SyncConfig, returning trigger and volatile.
func SyncFunction(schema, function string, version int, k, u string, c Conversion) (string, error) {
	source, err := SyncSource(version, k, u, c)
	if err != nil {
		return "", err
	}
	if !name.MatchString(schema) || !name.MatchString(function) {
		return "", fmt.Errorf("manifest: the sync function needs a schema and a name of at most 63 bytes")
	}
	return "CREATE FUNCTION " + QuoteIdent(schema) + "." + QuoteIdent(function) + "() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER VOLATILE SET search_path = pg_catalog, pg_temp AS $stego_sync$" +
		source + "$stego_sync$", nil
}

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
// the owner of the manifest, the oids of the tables of the manifest that are tables of declared entities, and the
// oids of the sync functions that an accepted sync trigger runs, which the Steps schema may hold.
type stepState struct {
	schema, owner string
	entities      map[uint32]bool
	accepted      map[uint32]bool
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

// syncPair finds the columns K and U, the version and the conversion for which SyncSource gives source, among the
// pairs that pairs gives; ok is false when none does.
func syncPair(source string, pairs [][2]string, columns map[string]tableColumn) (k, u string, version int, c Conversion, ok bool) {
	for _, pair := range pairs {
		for _, v := range SyncVersions {
			for _, conv := range SyncConversions {
				if !conv.fits(columns[pair[0]].typ, columns[pair[1]].typ) {
					continue
				}
				if text, err := SyncSource(v, pair[0], pair[1], conv); err == nil && text == source {
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
	k, u, _, _, ok := syncPair(tr.source, candidates, columns)
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
// manifest, which ends with the key id; with the tenant column of the table first when the table is owned, and then
// referring to the tenant column and the key id, so an owned table never refers to a key that is its key id alone;
// NO ACTION on delete and on update, match simple, validated and not deferrable; and named as ForeignKeyName names it.
func acceptForeignKey(ctx context.Context, tx *sql.Tx, t Table, rel uint32, n string, steps *stepState, columns map[string]tableColumn) (bool, error) {
	var from, to, ownKey, refKey, actions string
	var target uint32
	var validated, deferrable bool
	keyOf := `COALESCE((SELECT pg_catalog.string_agg(a.attname, chr(31) ORDER BY k.o) FROM pg_catalog.pg_constraint pk
    CROSS JOIN LATERAL pg_catalog.unnest(pk.conkey) WITH ORDINALITY AS k(n, o)
    JOIN pg_catalog.pg_attribute a ON a.attrelid = pk.conrelid AND a.attnum = k.n WHERE pk.conrelid = %s AND pk.contype = 'p'), '')`
	err := tx.QueryRowContext(ctx, `SELECT
  COALESCE((SELECT pg_catalog.string_agg(a.attname, chr(31) ORDER BY k.o) FROM pg_catalog.unnest(c.conkey) WITH ORDINALITY AS k(n, o)
    JOIN pg_catalog.pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.n), ''),
  COALESCE((SELECT pg_catalog.string_agg(a.attname, chr(31) ORDER BY k.o) FROM pg_catalog.unnest(c.confkey) WITH ORDINALITY AS k(n, o)
    JOIN pg_catalog.pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = k.n), ''),
  `+fmt.Sprintf(keyOf, "c.conrelid")+`, `+fmt.Sprintf(keyOf, "c.confrelid")+`,
  c.confrelid, c.confdeltype::text || c.confupdtype::text || c.confmatchtype::text, c.convalidated, c.condeferrable OR c.condeferred
  FROM pg_catalog.pg_constraint c WHERE c.conrelid = $1 AND c.conname = $2 AND c.contype = 'f'`, rel, n).Scan(
		&from, &to, &ownKey, &refKey, &target, &actions, &validated, &deferrable)
	if err != nil {
		return false, err
	}
	fromCols, refCols, ownCols := strings.Split(from, "\x1f"), strings.Split(refKey, "\x1f"), strings.Split(ownKey, "\x1f")
	column := fromCols[len(fromCols)-1]
	listed := slices.ContainsFunc(t.Columns, func(c Column) bool { return c.Name == column })
	c, ok := columns[column]
	switch {
	case !ok || listed || c.notNull || !c.builtin || !steps.entities[target] || actions != "aas" || !validated || deferrable:
		return false, nil
	case to != refKey || refCols[len(refCols)-1] != "id" || n != ForeignKeyName(t.Name, column):
		return false, nil
	}
	switch len(refCols) {
	case 1:
		// The referenced key is the key id alone: only a table that is not owned, whose primary key is its key id
		// alone, may refer to it, as an owned table must lead its foreign key with its tenant column.
		return len(fromCols) == 1 && len(ownCols) == 1 && ownCols[0] == "id", nil
	case 2:
		// The referenced key holds the tenant column: the table is owned, and its tenant column, the first column of
		// its primary key, comes first.
		return len(ownCols) == 2 && ownCols[1] == "id" && len(fromCols) == 2 && fromCols[0] == ownCols[0] && ownCols[0] == refCols[0], nil
	}
	return false, nil
}

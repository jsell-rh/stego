package postgresadapter

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"sort"
	"strings"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed conditions.go.tmpl
var conditionSource string

func conditionKeys(names []string) string {
	values := make([]string, len(names))
	for i, name := range names {
		values[i] = sqlLiteral(name)
	}
	return "ARRAY[" + strings.Join(values, ",") + "]::text[]"
}

func conditionBody(e types.Entity) string {
	owners := make([]string, 0, len(e.Conditions))
	for owner := range e.Conditions {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	var body strings.Builder
	body.WriteString(" IF TG_OP = 'INSERT' THEN NEW.stego_conditions := '{}'::jsonb; END IF;\n")
	fmt.Fprintf(&body, ` IF jsonb_typeof(NEW.stego_conditions) IS DISTINCT FROM 'object' OR octet_length(NEW.stego_conditions::text)>65536 OR NEW.stego_conditions - %s <> '{}'::jsonb THEN
 RAISE EXCEPTION 'invalid resource conditions' USING ERRCODE='23514'; END IF;
`, conditionKeys(owners))
	for _, owner := range owners {
		fmt.Fprintf(&body, ` IF NEW.stego_conditions ? %s THEN
 IF jsonb_typeof(NEW.stego_conditions->%s) IS DISTINCT FROM 'object' OR (NEW.stego_conditions->%s) - %s <> '{}'::jsonb THEN
 RAISE EXCEPTION 'invalid condition owner state' USING ERRCODE='23514'; END IF;
 END IF;
`, sqlLiteral(owner), sqlLiteral(owner), sqlLiteral(owner), conditionKeys(e.Conditions[owner]))
	}
	body.WriteString(` IF EXISTS(SELECT 1 FROM jsonb_each(NEW.stego_conditions) g CROSS JOIN LATERAL jsonb_each(g.value) c
 WHERE jsonb_typeof(c.value) IS DISTINCT FROM 'object'
 OR NOT (c.value ?& ARRAY['status','reason','message','observed_generation','last_transition_time'])
 OR c.value - ARRAY['status','reason','message','observed_generation','last_transition_time']::text[] <> '{}'::jsonb
 OR jsonb_typeof(c.value->'status') IS DISTINCT FROM 'string' OR c.value->>'status' NOT IN ('True','False','Unknown')
 OR jsonb_typeof(c.value->'reason') IS DISTINCT FROM 'string' OR c.value->>'reason' !~ '^[A-Z][A-Za-z0-9]{0,62}$'
 OR jsonb_typeof(c.value->'message') IS DISTINCT FROM 'string' OR octet_length(c.value->>'message')>1024 OR c.value->>'message' ~ '[[:cntrl:]]'
 OR jsonb_typeof(c.value->'observed_generation') IS DISTINCT FROM 'number' OR c.value->>'observed_generation' !~ '^[1-9][0-9]*$'
 OR jsonb_typeof(c.value->'last_transition_time') IS DISTINCT FROM 'string') THEN
 RAISE EXCEPTION 'invalid condition value' USING ERRCODE='23514'; END IF;
 IF EXISTS(SELECT 1 FROM jsonb_each(NEW.stego_conditions) g CROSS JOIN LATERAL jsonb_each(g.value) c
 WHERE (c.value->>'observed_generation')::bigint>NEW.stego_generation
 OR NOT isfinite((c.value->>'last_transition_time')::timestamptz)) THEN
 RAISE EXCEPTION 'invalid condition generation or time' USING ERRCODE='23514'; END IF;
`)
	return body.String()
}

func generateConditions(ctx gen.Context) ([]gen.File, error) {
	type owner struct {
		Name  string
		Types []string
	}
	type entity struct {
		Name, SQL string
		Owners    []owner
	}
	data := struct {
		Package, StorageImport string
		Entities               []entity
	}{Package: path.Base(ctx.OutputNamespace), StorageImport: ctx.StorageContract}
	for _, e := range ctx.Entities {
		if len(e.Conditions) == 0 {
			continue
		}
		entry := entity{Name: e.Name}
		owners := make([]string, 0, len(e.Conditions))
		for name := range e.Conditions {
			owners = append(owners, name)
		}
		sort.Strings(owners)
		for _, name := range owners {
			entry.Owners = append(entry.Owners, owner{Name: name, Types: e.Conditions[name]})
		}
		entry.SQL = fmt.Sprintf(`UPDATE %q SET stego_conditions=jsonb_set(stego_conditions,ARRAY[?]::text[],
 (SELECT jsonb_object_agg(item->>'name',jsonb_build_object('status',item->>'status','reason',item->>'reason','message',item->>'message',
 'observed_generation',stego_generation,'last_transition_time',CASE WHEN stego_conditions->?->(item->>'name')->>'status'=item->>'status'
 THEN stego_conditions->?->(item->>'name')->'last_transition_time' ELSE to_jsonb(statement_timestamp()) END)) FROM jsonb_array_elements(?::jsonb) item)),
 updated_time=now() WHERE id=? AND id COLLATE "C"=? AND stego_revision=? AND deleted_at IS NULL`, tableName(e.Name))
		data.Entities = append(data.Entities, entry)
	}
	if len(data.Entities) == 0 {
		return nil, nil
	}
	tmpl, err := template.New("conditions").Parse(conditionSource)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
		return nil, err
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format conditions: %w", err)
	}
	return []gen.File{{Path: path.Join(ctx.OutputNamespace, "conditions.go"), Content: code}}, nil
}

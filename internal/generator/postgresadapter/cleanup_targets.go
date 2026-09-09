package postgresadapter

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/types"
)

type cleanupTarget struct{ Owner, Field, GoField string }

func targetDefinitions(e types.Entity) []cleanupTarget {
	owners := make([]string, 0, len(e.CleanupTargets))
	for owner := range e.CleanupTargets {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	result := make([]cleanupTarget, 0, len(owners))
	for _, owner := range owners {
		result = append(result, cleanupTarget{owner, e.CleanupTargets[owner], toPascalCase(e.CleanupTargets[owner])})
	}
	return result
}

const targetDeclarations = `DECLARE
 target_owner text; target_field text; target_value text;
 target_state jsonb; target_reset boolean; target_keys text[];
 target_total integer := 0;
`

func targetContract(e types.Entity) (marker, body string) {
	definitions := targetDefinitions(e)
	encoded, _ := json.Marshal(definitions)
	marker = fmt.Sprintf("-- cleanup target fields %x", sha256.Sum256(encoded))
	var pairs, owners []string
	for _, d := range definitions {
		pairs = append(pairs, "("+sqlLiteral(d.Owner)+","+sqlLiteral(d.Field)+")")
		owners = append(owners, sqlLiteral(d.Owner))
	}
	body = fmt.Sprintf(`
 target_reset := TG_OP = 'INSERT';
 IF TG_OP = 'INSERT' THEN
  NEW.stego_cleanup_targets := '{}'::jsonb;
 ELSE
  target_reset := NEW.deleted_at IS NULL OR OLD.deleted_at IS NULL OR
   (to_jsonb(NEW) - %[1]s) IS DISTINCT FROM (to_jsonb(OLD) - %[1]s);
  IF target_reset THEN NEW.stego_cleanup_targets := OLD.stego_cleanup_targets; END IF;
 END IF;
 IF jsonb_typeof(NEW.stego_cleanup_targets) IS DISTINCT FROM 'object' THEN
  RAISE EXCEPTION 'invalid cleanup target state' USING ERRCODE='23514';
 END IF;
 FOR target_owner,target_field IN SELECT * FROM (VALUES %[2]s) AS fields(owner,field) LOOP
  target_value := to_jsonb(NEW)->>target_field;
  IF target_value IS NULL OR octet_length(target_value) NOT BETWEEN 1 AND 256 THEN
   RAISE EXCEPTION 'cleanup target must contain 1 through 256 bytes' USING ERRCODE='23514';
  END IF;
  IF TG_OP = 'INSERT' THEN target_state := '{}'::jsonb;
  ELSE
   target_state := NEW.stego_cleanup_targets->target_owner;
   IF jsonb_typeof(target_state) IS DISTINCT FROM 'object' OR jsonb_typeof(OLD.stego_cleanup_targets->target_owner) IS DISTINCT FROM 'object' THEN
    RAISE EXCEPTION 'missing cleanup target history' USING ERRCODE='23514';
   END IF;
   SELECT COALESCE(array_agg(key ORDER BY key COLLATE "C"),ARRAY[]::text[]) INTO target_keys FROM jsonb_object_keys(OLD.stego_cleanup_targets->target_owner) AS keys(key);
   IF NOT (target_state ?& target_keys) OR target_state - target_keys <> '{}'::jsonb THEN
    RAISE EXCEPTION 'cleanup target history is immutable' USING ERRCODE='23514';
   END IF;
  END IF;
  IF NOT (target_state ? target_value) THEN
   IF NOT target_reset THEN RAISE EXCEPTION 'current cleanup target is missing' USING ERRCODE='23514'; END IF;
   target_state := target_state || jsonb_build_object(target_value,false);
  END IF;
  IF target_reset THEN
   SELECT jsonb_object_agg(key,false) INTO target_state FROM jsonb_object_keys(target_state) AS keys(key);
  END IF;
  IF EXISTS(SELECT 1 FROM jsonb_each(target_state) WHERE octet_length(key) NOT BETWEEN 1 AND 256 OR jsonb_typeof(value) IS DISTINCT FROM 'boolean') THEN
   RAISE EXCEPTION 'invalid cleanup target observation' USING ERRCODE='23514';
  END IF;
  target_total := target_total + (SELECT count(*) FROM jsonb_object_keys(target_state));
  NEW.stego_cleanup_targets := jsonb_set(NEW.stego_cleanup_targets,ARRAY[target_owner],target_state);
  NEW.stego_cleanup := jsonb_set(NEW.stego_cleanup,ARRAY[target_owner],to_jsonb(NEW.deleted_at IS NOT NULL AND (SELECT bool_and(value='true'::jsonb) FROM jsonb_each(target_state))));
 END LOOP;
 IF NOT (NEW.stego_cleanup_targets ?& ARRAY[%[3]s]::text[]) OR NEW.stego_cleanup_targets - ARRAY[%[3]s]::text[] <> '{}'::jsonb OR target_total>128 OR octet_length(NEW.stego_cleanup_targets::text)>65536 THEN
  RAISE EXCEPTION 'invalid or excessive cleanup target history' USING ERRCODE='23514';
 END IF;
 %[4]s
`, cleanupIgnored(e), strings.Join(pairs, ","), strings.Join(owners, ","), marker)
	return
}

func targetMigration(e types.Entity, table, marker string) []string {
	if len(e.CleanupTargets) == 0 {
		return []string{fmt.Sprintf(`DO $targets$ BEGIN
 IF EXISTS(SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid=%s::regclass AND attname='stego_cleanup_targets' AND NOT attisdropped) THEN
  IF EXISTS(SELECT 1 FROM %q WHERE stego_cleanup_targets<>'{}'::jsonb) THEN
   RAISE EXCEPTION 'cleanup target declarations cannot be removed from retained resources';
  END IF;
 END IF;
 END; $targets$;
`, sqlLiteral(table), table)}
	}
	return []string{fmt.Sprintf("ALTER TABLE %q ADD COLUMN IF NOT EXISTS stego_cleanup_targets jsonb NOT NULL DEFAULT '{}';\n", table), fmt.Sprintf(`DO $targets$ BEGIN
 IF EXISTS(SELECT 1 FROM %q) AND NOT EXISTS(
  SELECT 1 FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_proc p ON p.oid=t.tgfoid
  WHERE t.tgrelid=%s::regclass AND t.tgname='stego_resource_revision' AND strpos(p.prosrc,%s)>0
 ) THEN
  RAISE EXCEPTION 'cleanup target history requires an explicit migration for existing resources';
 END IF;
END; $targets$;
`, table, sqlLiteral(table), sqlLiteral(marker))}
}

func resetTargetObservations() string {
	return `stego_cleanup_targets=(SELECT jsonb_object_agg(owner.key,(SELECT jsonb_object_agg(target.key,false) FROM jsonb_object_keys(owner.value) AS target(key))) FROM jsonb_each(stego_cleanup_targets) AS owner)`
}

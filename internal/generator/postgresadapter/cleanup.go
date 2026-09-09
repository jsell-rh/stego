package postgresadapter

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jsell-rh/stego/internal/types"
)

func cleanupIgnored(entity types.Entity) string {
	names := "'stego_revision','stego_generation','stego_observations','stego_cleanup','updated_time'"
	if len(entity.CleanupTargets) > 0 {
		names += ",'stego_cleanup_targets'"
	}
	return "ARRAY[" + names + "]"
}

func cleanupContract(entity types.Entity) (owners []string, initial, keys, body string) {
	owners = slices.Clone(entity.CleanupOwners)
	slices.Sort(owners)
	state := map[string]bool{}
	var literals []string
	for _, owner := range owners {
		state[owner] = false
		literals = append(literals, sqlLiteral(owner))
	}
	encoded, _ := json.Marshal(state)
	initial = sqlLiteral(string(encoded)) + "::jsonb"
	keys = "ARRAY[" + strings.Join(literals, ",") + "]::text[]"
	if len(owners) == 0 {
		return
	}
	body = fmt.Sprintf(`
 IF TG_OP = 'INSERT' THEN
  NEW.stego_cleanup := %[1]s;
 ELSE
  IF NEW.deleted_at IS NULL OR OLD.deleted_at IS NULL OR
   (to_jsonb(NEW) - %[3]s) IS DISTINCT FROM
   (to_jsonb(OLD) - %[3]s) THEN
   NEW.stego_cleanup := %[1]s;
  ELSE
   IF jsonb_typeof(NEW.stego_cleanup) IS DISTINCT FROM 'object' THEN
    RAISE EXCEPTION 'invalid cleanup state' USING ERRCODE = '23514';
   END IF;
   IF NOT (NEW.stego_cleanup ?& %[2]s) OR NEW.stego_cleanup - %[2]s <> '{}'::jsonb OR
    EXISTS (SELECT 1 FROM jsonb_each(NEW.stego_cleanup) WHERE jsonb_typeof(value) IS DISTINCT FROM 'boolean') THEN
    RAISE EXCEPTION 'invalid cleanup owners or observations' USING ERRCODE = '23514';
   END IF;
  END IF;
 END IF;
`, initial, keys, cleanupIgnored(entity))
	return
}

package postgresadapter

import (
	"encoding/json"
	"fmt"

	"github.com/jsell-rh/stego/internal/types"
)

func cleanupCompletion(entity types.Entity) string {
	state := map[string]bool{}
	for _, owner := range entity.CleanupOwners {
		state[owner] = true
	}
	data, _ := json.Marshal(state)
	return string(data)
}

// This column is excluded from AutoMigrate. Its first migration preserves the
// old visibility of retained deletions, including those with unfinished cleanup.
// Later migrations must not finalize requests accepted under the new contract.
func finalizationMigration(table string) string {
	return fmt.Sprintf(`DO $finalization$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid=%s::regclass AND attname='stego_finalized_at' AND NOT attisdropped) THEN
  ALTER TABLE %q ADD COLUMN stego_finalized_at timestamptz;
  UPDATE %q SET stego_finalized_at=deleted_at WHERE deleted_at IS NOT NULL;
 END IF;
 END; $finalization$;
`, sqlLiteral(table), table, table)
}

func finalizationBody(complete string) string {
	return fmt.Sprintf(`
 IF TG_OP = 'INSERT' THEN
  IF NEW.stego_finalized_at IS NOT NULL THEN
   RAISE EXCEPTION 'new resource cannot be finalized' USING ERRCODE = '23514';
  END IF;
 ELSE
  IF OLD.stego_finalized_at IS NOT NULL THEN
   IF NEW.stego_finalized_at IS DISTINCT FROM OLD.stego_finalized_at THEN
    RAISE EXCEPTION 'resource finalization is permanent' USING ERRCODE = '23514';
   END IF;
  ELSIF NEW.stego_finalized_at IS NOT NULL THEN
   IF NEW.deleted_at IS NULL OR NEW.stego_cleanup IS DISTINCT FROM %s::jsonb THEN
    RAISE EXCEPTION 'resource cleanup is not complete' USING ERRCODE = '23514';
   END IF;
   NEW.stego_finalized_at := clock_timestamp();
  END IF;
 END IF;
`, sqlLiteral(complete))
}

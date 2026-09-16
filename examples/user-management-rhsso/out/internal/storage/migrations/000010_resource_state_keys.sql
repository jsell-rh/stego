BEGIN;
SET LOCAL lock_timeout='5000';
SET LOCAL statement_timeout='25000';
CREATE INDEX IF NOT EXISTS stego_resource_state_scope_idx ON stego_resource_state USING btree(entity,scope,resource_id);
COMMIT;

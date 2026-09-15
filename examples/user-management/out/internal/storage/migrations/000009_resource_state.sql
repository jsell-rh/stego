BEGIN;
CREATE TABLE IF NOT EXISTS stego_resource_state (
 entity text COLLATE "C" NOT NULL,
 resource_id text COLLATE "C" NOT NULL,
 scope text COLLATE "C" NOT NULL,
 data bytea NOT NULL,
 version bigint NOT NULL,
 PRIMARY KEY(entity,resource_id,scope),
 CHECK(octet_length(entity) BETWEEN 1 AND 256),
 CHECK(octet_length(resource_id) BETWEEN 1 AND 256),
 CHECK(octet_length(scope) BETWEEN 1 AND 128),
 CHECK(octet_length(data) <= 65536),
 CHECK(version > 0)
);
CREATE OR REPLACE FUNCTION stego_guard_resource_state() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $guard$
BEGIN
 IF TG_OP = 'DELETE' THEN
  RAISE EXCEPTION 'resource state history cannot be removed' USING ERRCODE = '23514';
 END IF;
 IF NEW.entity IS DISTINCT FROM OLD.entity OR NEW.resource_id IS DISTINCT FROM OLD.resource_id
    OR NEW.scope IS DISTINCT FROM OLD.scope OR OLD.version = 9223372036854775807
    OR NEW.version IS DISTINCT FROM OLD.version + 1 THEN
  RAISE EXCEPTION 'resource state identity or version differs' USING ERRCODE = '23514';
 END IF;
 RETURN NEW;
END;
$guard$;
DROP TRIGGER IF EXISTS stego_resource_state_guard ON stego_resource_state;
CREATE TRIGGER stego_resource_state_guard BEFORE UPDATE OR DELETE ON stego_resource_state FOR EACH ROW EXECUTE FUNCTION stego_guard_resource_state();
COMMIT;

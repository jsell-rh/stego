BEGIN;
CREATE TABLE IF NOT EXISTS stego_effect_bindings (
 entity text COLLATE "C" NOT NULL,
 resource_id text COLLATE "C" NOT NULL,
 scope text COLLATE "C" NOT NULL,
 digest text COLLATE "C" NOT NULL,
 closed boolean NOT NULL,
 PRIMARY KEY(entity,resource_id,scope),
 CHECK(octet_length(entity) BETWEEN 1 AND 256),
 CHECK(octet_length(resource_id) BETWEEN 1 AND 256),
 CHECK(octet_length(scope) BETWEEN 1 AND 128),
 CHECK(digest ~ '^[0-9a-f]{64}$' OR (closed AND digest=''))
);
CREATE OR REPLACE FUNCTION stego_guard_effect_binding() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $guard$
BEGIN
 IF TG_OP = 'DELETE' THEN
  RAISE EXCEPTION 'effect binding history cannot be removed' USING ERRCODE = '23514';
 END IF;
 IF NEW.entity IS DISTINCT FROM OLD.entity OR NEW.resource_id IS DISTINCT FROM OLD.resource_id
    OR NEW.scope IS DISTINCT FROM OLD.scope OR NEW.digest IS DISTINCT FROM OLD.digest
    OR (OLD.closed AND NOT NEW.closed) THEN
  RAISE EXCEPTION 'effect binding identity and closure are immutable' USING ERRCODE = '23514';
 END IF;
 RETURN NEW;
END;
$guard$;
DROP TRIGGER IF EXISTS stego_effect_binding_guard ON stego_effect_bindings;
CREATE TRIGGER stego_effect_binding_guard BEFORE UPDATE OR DELETE ON stego_effect_bindings FOR EACH ROW EXECUTE FUNCTION stego_guard_effect_binding();
COMMIT;

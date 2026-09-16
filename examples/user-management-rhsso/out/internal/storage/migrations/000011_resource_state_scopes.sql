BEGIN;
SET LOCAL lock_timeout='5000';
SET LOCAL statement_timeout='25000';
CREATE TABLE IF NOT EXISTS stego_resource_state_scopes (
 entity text COLLATE "C" NOT NULL, scope text COLLATE "C" NOT NULL,
 revision bigint NOT NULL, sealed boolean NOT NULL,
 PRIMARY KEY(entity,scope),
 CHECK(octet_length(entity) BETWEEN 1 AND 256),
 CHECK(octet_length(scope) BETWEEN 1 AND 128), CHECK(revision>=0), CHECK(NOT sealed OR revision>0)
);
LOCK TABLE stego_resource_state IN SHARE ROW EXCLUSIVE MODE;
INSERT INTO stego_resource_state_scopes(entity,scope,revision,sealed) SELECT entity,scope,count(*),false FROM stego_resource_state GROUP BY entity,scope ON CONFLICT(entity,scope) DO NOTHING;
CREATE OR REPLACE FUNCTION stego_guard_resource_state_scope() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $guard$
BEGIN
 IF TG_OP = 'DELETE' THEN
  RAISE EXCEPTION 'resource state scope history cannot be removed' USING ERRCODE='23514';
 END IF;
 IF NEW.entity IS DISTINCT FROM OLD.entity OR NEW.scope IS DISTINCT FROM OLD.scope
    OR OLD.sealed OR OLD.revision=9223372036854775807 OR NEW.revision IS DISTINCT FROM OLD.revision+1 THEN
  RAISE EXCEPTION 'resource state scope is closed or its revision differs' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$guard$;
DROP TRIGGER IF EXISTS stego_resource_state_scope_guard ON stego_resource_state_scopes;
CREATE TRIGGER stego_resource_state_scope_guard BEFORE UPDATE OR DELETE ON stego_resource_state_scopes FOR EACH ROW EXECUTE FUNCTION stego_guard_resource_state_scope();
CREATE OR REPLACE FUNCTION stego_track_resource_state_keys() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $guard$
DECLARE current_revision bigint; current_sealed boolean; existing_key boolean;
BEGIN
 EXECUTE format('INSERT INTO %I.stego_resource_state_scopes(entity,scope,revision,sealed) VALUES($1,$2,0,false) ON CONFLICT(entity,scope) DO NOTHING',TG_TABLE_SCHEMA)
  USING NEW.entity,NEW.scope;
 EXECUTE format('SELECT revision,sealed FROM %I.stego_resource_state_scopes WHERE entity=$1 AND scope=$2 FOR UPDATE',TG_TABLE_SCHEMA)
  INTO STRICT current_revision,current_sealed USING NEW.entity,NEW.scope;
 EXECUTE format('SELECT EXISTS(SELECT 1 FROM %I.stego_resource_state WHERE entity=$1 AND resource_id=$2 AND scope=$3)',TG_TABLE_SCHEMA)
  INTO existing_key USING NEW.entity,NEW.resource_id,NEW.scope;
 IF existing_key THEN RETURN NEW; END IF;
 IF current_sealed THEN
  RAISE EXCEPTION 'resource state scope is closed' USING ERRCODE='23514',CONSTRAINT='stego_resource_state_scope_closed';
 END IF;
 IF current_revision=9223372036854775807 THEN
  RAISE EXCEPTION 'resource state scope revision is exhausted' USING ERRCODE='23514';
 END IF;
 EXECUTE format('UPDATE %I.stego_resource_state_scopes SET revision=revision+1 WHERE entity=$1 AND scope=$2',TG_TABLE_SCHEMA)
  USING NEW.entity,NEW.scope;
 RETURN NEW;
END;
$guard$;
DROP TRIGGER IF EXISTS stego_resource_state_key_guard ON stego_resource_state;
CREATE TRIGGER stego_resource_state_key_guard BEFORE INSERT ON stego_resource_state FOR EACH ROW EXECUTE FUNCTION stego_track_resource_state_keys();
COMMIT;

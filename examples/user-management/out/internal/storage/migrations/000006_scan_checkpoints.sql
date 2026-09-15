BEGIN;
CREATE TABLE IF NOT EXISTS stego_scan_checkpoints (
 entity text COLLATE "C" NOT NULL,
 resource_id text COLLATE "C" NOT NULL,
 scope text COLLATE "C" NOT NULL,
 after_cursor text COLLATE "C" NOT NULL,
 version bigint NOT NULL,
 PRIMARY KEY(entity, resource_id, scope),
 CHECK(octet_length(entity) BETWEEN 1 AND 256),
 CHECK(octet_length(resource_id) BETWEEN 1 AND 256),
 CHECK(octet_length(scope) BETWEEN 1 AND 128),
 CHECK(octet_length(after_cursor) <= 1024),
 CHECK(version > 0)
);
COMMIT;

package idempotency

// Migration is the DDL text that creates the key table idempotency_keys (ST-SUP-RP07-LC01). A service applies it
// with its own migrations. The primary key (tenant_id, principal_id, key) scopes a key to one client, so one
// client's key never collides with another's. A key is at most 255 characters, and status is in_progress or
// completed. response_headers keeps the stored headers in a length-prefixed binary form, and response_body keeps
// the stored body as bytes, so a replay gives both back byte for byte. The index on expires_at serves Purge.
const Migration = `CREATE TABLE IF NOT EXISTS idempotency_keys (
	tenant_id text NOT NULL,
	principal_id text NOT NULL,
	key varchar(255) NOT NULL,
	fingerprint text NOT NULL,
	status text NOT NULL CHECK (status IN ('in_progress', 'completed')),
	response_code integer,
	response_headers bytea,
	response_body bytea,
	locked_at timestamptz NOT NULL,
	expires_at timestamptz NOT NULL,
	PRIMARY KEY (tenant_id, principal_id, key),
	CHECK (status <> 'completed' OR response_code IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS idempotency_keys_expires_at ON idempotency_keys (expires_at);
`

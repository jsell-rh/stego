-- Apply this migration through the reviewed deployment migration process.
CREATE TABLE stego_browser_sessions (
    id_hash bytea PRIMARY KEY CHECK (octet_length(id_hash) = 32),
    payload bytea NOT NULL CHECK (octet_length(payload) BETWEEN 28 AND 65536),
    state text NOT NULL CHECK (state IN ('login', 'active', 'refreshing')),
    expires_at timestamptz NOT NULL,
    changed_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX stego_browser_sessions_expiry ON stego_browser_sessions (expires_at);

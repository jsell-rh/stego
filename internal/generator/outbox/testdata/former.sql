-- Apply once through the application's migration process.
CREATE SCHEMA stego_outbox;

CREATE TABLE stego_outbox.messages (
    id uuid PRIMARY KEY,
    sequence bigint GENERATED ALWAYS AS IDENTITY UNIQUE NOT NULL,
    destination text NOT NULL CHECK (destination ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$'),
    resource_key text NOT NULL CHECK (octet_length(resource_key) BETWEEN 1 AND 256),
    kind text NOT NULL CHECK (kind ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$'),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object' AND octet_length(payload::text) <= 131072),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    attempts bigint NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    lease_token uuid,
    lease_until timestamptz,
    failure_code text NOT NULL DEFAULT '' CHECK (octet_length(failure_code) <= 128),
    CHECK ((lease_token IS NULL) = (lease_until IS NULL))
);

CREATE INDEX messages_resource_order ON stego_outbox.messages (destination, resource_key, sequence);
CREATE INDEX messages_available ON stego_outbox.messages (available_at, sequence);

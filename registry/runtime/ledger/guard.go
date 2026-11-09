package ledger

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The tables and the functions of the ledger. Every declared ledger of a service shares the four tables, and each
// row carries its ledger_id. Each table is tenant-owned (ST-STD-TXN-R01). The runtime role reads them and writes
// them only through the owner functions (ST-SUP-RP08-LC09).
const (
	// AccountsTable holds one row for each account: its normal side, its currency, and its balance limit or hot
	// flag.
	AccountsTable = "accounts"
	// TransfersTable holds one row for each transfer, pending transfer, post or void of a pending transfer, and
	// reversal.
	TransfersTable = "transfers"
	// EntriesTable holds the debit and credit entries of each transfer.
	EntriesTable = "entries"
	// SnapshotsTable holds the balance snapshots that ledger_snapshot writes.
	SnapshotsTable = "balance_snapshots"
	// GuardSetting is the transaction-local setting that an owner function sets, while its one UPDATE of a
	// transfer runs, to the change that the guard trigger lets through. The function restores it after.
	GuardSetting = "stego.ledger_change"
)

// OwnerFunctions are the SECURITY DEFINER functions that the runtime role may run. They are the only way the runtime
// role writes the ledger tables.
var OwnerFunctions = []string{
	"ledger_open(text, text, text, text, text, text, bigint, boolean)",
	"ledger_post(text, text, text, text, jsonb, timestamptz)",
	"ledger_pend(text, text, text, text, jsonb, timestamptz)",
	"ledger_post_pending(text, text, text, uuid, jsonb)",
	"ledger_void(text, text, text, uuid)",
	"ledger_reverse(text, text, text, uuid, jsonb)",
	"ledger_snapshot(text, text, bigint)",
}

// innerFunctions are the functions that only the owner functions run; PUBLIC and the runtime role may not.
var innerFunctions = []string{
	"ledger_guard()", "ledger_check_entries()", "ledger_check_transfers()", "ledger_check_change()",
	"ledger_declared(text)", "ledger_call(text, text)", "ledger_text(text, text)", "ledger_key(text, text)",
	"ledger_fingerprint(text, text, jsonb, timestamptz, uuid, uuid, bigint)", "ledger_replay(text, text, text)",
	"ledger_entries(text, text, text, jsonb)", "ledger_holds(text, text)", "ledger_lock_limited(text, text, jsonb)",
	"ledger_limit(text, text, jsonb, boolean)",
	"ledger_insert(text, text, uuid, text, text, text, text, bigint, text, text, uuid, uuid, timestamptz, timestamptz, jsonb)",
	"ledger_write(text, text, text, text, text, jsonb, timestamptz)",
	"ledger_settle(text, text, text, uuid, jsonb, text)",
}

// SQLSTATE codes that the owner functions raise. They are the class LG, which PostgreSQL does not use.
const (
	codeAlreadyPosted   = "LG001"
	codeAlreadyVoided   = "LG002"
	codeAlreadyReversed = "LG003"
	codeNotPosted       = "LG004"
	codeNoPending       = "LG005"
	codeKeyReused       = "LG006"
	codeBelowMinimum    = "LG007"
	codeExpired         = "LG008"
	codeExceedsPending  = "LG009"
	codeEffective       = "LG010"
	codeIsolation       = "LG011"
	codeNotSplit        = "LG012"
	codeNotMirror       = "LG013"
)

var plainName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Config is what the DDL text needs. The table owner runs the text in the migration job, as the owner role, so the
// tables and the functions belong to that role (ST-SUP-RP04-LC06).
type Config struct {
	// Schema holds the ledger tables; the empty schema is public.
	Schema string
	// RuntimeRole is the role of the running service.
	RuntimeRole string
	// Ledgers are the declared ledgers. The owner functions read their currency, close date, pending timeout and
	// snapshot interval from the text, so a caller cannot give other values.
	Ledgers []Settings
}

// DDL returns the statements that create the ledger (ST-SUP-RP08-LC09, ST-STD-TXN-R13): the tables accounts,
// transfers, entries and balance_snapshots, tenant-owned with forced row-level security, and balance_snapshots
// with no foreign key to accounts; statement triggers that refuse entries that do not balance or are not inserted
// with their transfer; the guard trigger BEFORE UPDATE OR DELETE (and TRUNCATE) on transfers, entries and accounts,
// which lets through only the one change of status from pending and the one reversed_by that the owner functions
// make, each checked at commit against the new row that names it; the owner functions, SECURITY DEFINER with
// search_path pg_catalog, pg_temp, which check the tenant, the isolation and every argument and enforce the
// balance, the accounts, the currency, the limit under the row lock, the split of a post, the mirror of a
// reversal and the expiry by the database clock, and compute each snapshot; and the grants: SELECT only on the
// four tables, and EXECUTE on the owner functions and ledger_balance, for the runtime role.
//
// The generated migration adds the constraints that vary with the declaration and need no function
// (ST-STD-RUNTIME-R05): the CHECK constraints of the declared transfer codes.
func DDL(c Config) (string, error) {
	if c.Schema == "" {
		c.Schema = "public"
	}
	if !plainName.MatchString(c.Schema) || !plainName.MatchString(c.RuntimeRole) {
		return "", errors.New("ledger: the schema and the runtime role must be plain lower-case names")
	}
	if len(c.Ledgers) == 0 {
		return "", errors.New("ledger: the DDL text needs at least one declared ledger")
	}
	var rows []string
	seen := map[string]bool{}
	for _, s := range c.Ledgers {
		l, err := New(s)
		if err != nil {
			return "", err
		}
		if seen[l.id] {
			return "", fmt.Errorf("ledger: the ledger %s is declared twice", l.id)
		}
		seen[l.id] = true
		closed := "'-infinity'::timestamptz"
		if !l.closed.IsZero() {
			closed = literal(l.closed.UTC().Format(time.RFC3339Nano)) + "::timestamptz"
		}
		rows = append(rows, fmt.Sprintf("(%s::text, %s::text, %s, interval '%d microseconds', %d::bigint)",
			literal(l.id), literal(l.currency.Code()), closed, l.timeout.Microseconds(), max(l.every, 1)))
	}
	return strings.NewReplacer(
		"{s}", c.Schema,
		"{role}", c.RuntimeRole,
		"{declared}", strings.Join(rows, ",\n    "),
		"{accounts}", AccountsTable,
		"{transfers}", TransfersTable,
		"{entries}", EntriesTable,
		"{snapshots}", SnapshotsTable,
		"{change}", GuardSetting,
		"{posted}", codeAlreadyPosted,
		"{voided}", codeAlreadyVoided,
		"{reversed}", codeAlreadyReversed,
		"{notposted}", codeNotPosted,
		"{nopending}", codeNoPending,
		"{reused}", codeKeyReused,
		"{belowmin}", codeBelowMinimum,
		"{expired}", codeExpired,
		"{exceeds}", codeExceedsPending,
		"{effective}", codeEffective,
		"{isolation}", codeIsolation,
		"{notsplit}", codeNotSplit,
		"{notmirror}", codeNotMirror,
		"{grants}", strings.Join(qualified(c.Schema, OwnerFunctions), ", "),
		"{inner}", strings.Join(qualified(c.Schema, innerFunctions), ", "),
	).Replace(ddlText), nil
}

// literal quotes s as an SQL string literal (standard_conforming_strings is on since PostgreSQL 9.1).
func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func qualified(schema string, fns []string) []string {
	out := make([]string, len(fns))
	for i, f := range fns {
		out[i] = schema + "." + f
	}
	return out
}

// signedSQL is an entry's amount on the normal side of its account: positive on that side, negative on the other
// (ST-SUP-RP08-LC07).
const signedSQL = `CASE WHEN e.side = a.normal THEN e.amount ELSE -e.amount END`

const ddlText = `CREATE TABLE {s}.{accounts} (
  tenant_id text NOT NULL,
  ledger_id text NOT NULL,
  id text NOT NULL,
  code text NOT NULL,
  normal text NOT NULL CHECK (normal IN ('debit', 'credit')),
  currency char(3) NOT NULL,
  min_balance bigint,
  hot boolean NOT NULL DEFAULT false,
  opened_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, ledger_id, id),
  CHECK (NOT (hot AND min_balance IS NOT NULL))
);
CREATE TABLE {s}.{transfers} (
  tenant_id text NOT NULL,
  ledger_id text NOT NULL,
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  idempotency_key text NOT NULL,
  fingerprint text NOT NULL,
  kind text NOT NULL CHECK (kind IN ('transfer', 'pending', 'post', 'void', 'reversal')),
  code text NOT NULL,
  amount bigint NOT NULL CHECK (amount > 0),
  currency char(3) NOT NULL,
  status text NOT NULL CHECK (status IN ('pending', 'posted', 'voided')),
  pending_id uuid,
  reverses uuid,
  reversed_by uuid,
  expires_at timestamptz,
  effective_at timestamptz NOT NULL,
  posted_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, ledger_id, id),
  UNIQUE (ledger_id, idempotency_key),
  FOREIGN KEY (tenant_id, ledger_id, pending_id) REFERENCES {s}.{transfers} (tenant_id, ledger_id, id),
  FOREIGN KEY (tenant_id, ledger_id, reverses) REFERENCES {s}.{transfers} (tenant_id, ledger_id, id),
  CHECK ((kind IN ('post', 'void')) = (pending_id IS NOT NULL)),
  CHECK ((kind = 'reversal') = (reverses IS NOT NULL)),
  CHECK ((kind = 'pending') = (expires_at IS NOT NULL)),
  CHECK (kind = 'pending' OR status = CASE kind WHEN 'void' THEN 'voided' ELSE 'posted' END),
  CHECK (reversed_by IS NULL OR kind IN ('transfer', 'post', 'reversal'))
);
CREATE INDEX {transfers}_open_pending ON {s}.{transfers} (tenant_id, ledger_id, expires_at) WHERE status = 'pending';
CREATE TABLE {s}.{entries} (
  tenant_id text NOT NULL,
  ledger_id text NOT NULL,
  transfer_id uuid NOT NULL,
  seq integer NOT NULL,
  account_id text NOT NULL,
  side text NOT NULL CHECK (side IN ('debit', 'credit')),
  amount bigint NOT NULL CHECK (amount > 0),
  currency char(3) NOT NULL,
  pending boolean NOT NULL,
  effective_at timestamptz NOT NULL,
  posted_at timestamptz NOT NULL,
  txid xid8 NOT NULL DEFAULT pg_current_xact_id(),
  PRIMARY KEY (tenant_id, ledger_id, transfer_id, seq),
  FOREIGN KEY (tenant_id, ledger_id, transfer_id) REFERENCES {s}.{transfers} (tenant_id, ledger_id, id)
);
CREATE INDEX {entries}_account_txid ON {s}.{entries} (tenant_id, ledger_id, account_id, txid);
CREATE INDEX {entries}_account_effective ON {s}.{entries} (tenant_id, ledger_id, account_id, effective_at);
CREATE INDEX {entries}_holds ON {s}.{entries} (tenant_id, ledger_id, account_id, transfer_id) WHERE pending;
CREATE TABLE {s}.{snapshots} (
  tenant_id text NOT NULL,
  ledger_id text NOT NULL,
  account_id text NOT NULL,
  through_xid xid8 NOT NULL,
  amount bigint NOT NULL,
  entry_count bigint NOT NULL,
  fence_token bigint NOT NULL,
  taken_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, ledger_id, account_id, through_xid)
);
CREATE FUNCTION {s}.ledger_guard() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  IF TG_OP = 'UPDATE' AND TG_TABLE_NAME = '{transfers}' THEN
    IF current_setting('{change}', true) = 'settle:' || OLD.id::text AND OLD.kind = 'pending'
        AND OLD.status = 'pending' AND NEW.status IN ('posted', 'voided')
        AND (to_jsonb(NEW) - 'status') = (to_jsonb(OLD) - 'status') THEN
      RETURN NEW;
    END IF;
    IF current_setting('{change}', true) = 'reverse:' || OLD.id::text AND OLD.status = 'posted'
        AND OLD.reversed_by IS NULL AND NEW.reversed_by IS NOT NULL
        AND (to_jsonb(NEW) - 'reversed_by') = (to_jsonb(OLD) - 'reversed_by') THEN
      RETURN NEW;
    END IF;
  END IF;
  RAISE EXCEPTION 'the ledger table % rejects %', TG_TABLE_NAME, TG_OP USING ERRCODE = 'insufficient_privilege';
END
$$;
CREATE TRIGGER {transfers}_guard BEFORE UPDATE OR DELETE ON {s}.{transfers}
  FOR EACH ROW EXECUTE FUNCTION {s}.ledger_guard();
CREATE TRIGGER {transfers}_guard_truncate BEFORE TRUNCATE ON {s}.{transfers}
  FOR EACH STATEMENT EXECUTE FUNCTION {s}.ledger_guard();
CREATE TRIGGER {entries}_guard BEFORE UPDATE OR DELETE ON {s}.{entries}
  FOR EACH ROW EXECUTE FUNCTION {s}.ledger_guard();
CREATE TRIGGER {entries}_guard_truncate BEFORE TRUNCATE ON {s}.{entries}
  FOR EACH STATEMENT EXECUTE FUNCTION {s}.ledger_guard();
CREATE TRIGGER {accounts}_guard BEFORE UPDATE OR DELETE ON {s}.{accounts}
  FOR EACH ROW EXECUTE FUNCTION {s}.ledger_guard();
CREATE TRIGGER {accounts}_guard_truncate BEFORE TRUNCATE ON {s}.{accounts}
  FOR EACH STATEMENT EXECUTE FUNCTION {s}.ledger_guard();
CREATE FUNCTION {s}.ledger_check_entries() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  bad record;
BEGIN
  SELECT x.transfer_id, x.why INTO bad FROM (
    SELECT a.transfer_id, CASE
      WHEN t.id IS NULL THEN 'names no transfer'
      WHEN t.kind = 'void' THEN 'is a void, which has no entries'
      WHEN count(*) < 2 THEN 'has fewer than two entries'
      WHEN count(*) <> (SELECT count(*) FROM {s}.{entries} e WHERE e.tenant_id = a.tenant_id
          AND e.ledger_id = a.ledger_id AND e.transfer_id = a.transfer_id)
        THEN 'has entries that were not inserted in one statement'
      WHEN sum(CASE a.side WHEN 'debit' THEN a.amount ELSE 0 END)
          <> sum(CASE a.side WHEN 'credit' THEN a.amount ELSE 0 END) THEN 'does not balance'
      WHEN sum(CASE a.side WHEN 'debit' THEN a.amount ELSE 0 END) <> t.amount THEN 'differs from its amount'
      WHEN bool_or(a.currency <> t.currency) THEN 'has an entry in another currency than the transfer'
      WHEN bool_or(a.pending <> (t.kind = 'pending')) THEN 'has an entry with the wrong pending flag'
      WHEN bool_or(a.effective_at <> t.effective_at OR a.posted_at <> t.posted_at)
        THEN 'has an entry with other times than the transfer'
      WHEN bool_or(a.txid <> pg_current_xact_id()) THEN 'has an entry with another transaction ID'
    END AS why
    FROM added a LEFT JOIN {s}.{transfers} t
      ON t.tenant_id = a.tenant_id AND t.ledger_id = a.ledger_id AND t.id = a.transfer_id
    GROUP BY a.tenant_id, a.ledger_id, a.transfer_id, t.id, t.kind, t.amount, t.currency, t.effective_at, t.posted_at
  ) x WHERE x.why IS NOT NULL LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION 'ledger: the transfer % %', bad.transfer_id, bad.why USING ERRCODE = 'check_violation';
  END IF;
  SELECT a.transfer_id, a.account_id, a.currency INTO bad FROM added a LEFT JOIN {s}.{accounts} c
    ON c.tenant_id = a.tenant_id AND c.ledger_id = a.ledger_id AND c.id = a.account_id
    WHERE c.id IS NULL OR c.currency <> a.currency LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION 'ledger: the transfer % has an entry in % on %, which is not an account of the ledger in that currency',
      bad.transfer_id, bad.currency, bad.account_id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END
$$;
CREATE TRIGGER {entries}_balance AFTER INSERT ON {s}.{entries} REFERENCING NEW TABLE AS added
  FOR EACH STATEMENT EXECUTE FUNCTION {s}.ledger_check_entries();
CREATE FUNCTION {s}.ledger_check_transfers() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  bad record;
BEGIN
  SELECT x.id, x.why INTO bad FROM (
    SELECT a.id, CASE
      WHEN a.posted_at <> now() THEN 'is not posted at the time of its transaction'
      WHEN a.status <> CASE a.kind WHEN 'pending' THEN 'pending' WHEN 'void' THEN 'voided' ELSE 'posted' END
        THEN 'has a status that its kind does not start with'
      WHEN a.reversed_by IS NOT NULL THEN 'is inserted as reversed'
      WHEN a.kind <> 'void' AND (SELECT count(*) FROM {s}.{entries} e WHERE e.tenant_id = a.tenant_id
          AND e.ledger_id = a.ledger_id AND e.transfer_id = a.id) < 2 THEN 'has fewer than two entries'
      WHEN a.kind IN ('post', 'void') AND NOT EXISTS (SELECT 1 FROM {s}.{transfers} p
          WHERE p.tenant_id = a.tenant_id AND p.ledger_id = a.ledger_id AND p.id = a.pending_id
            AND p.kind = 'pending' AND p.currency = a.currency AND a.amount <= p.amount
            AND p.status = CASE a.kind WHEN 'post' THEN 'posted' ELSE 'voided' END)
        THEN 'does not settle a pending transfer through the post and void functions'
      WHEN a.kind IN ('post', 'void') AND (SELECT count(*) FROM {s}.{transfers} o WHERE o.tenant_id = a.tenant_id
          AND o.ledger_id = a.ledger_id AND o.pending_id = a.pending_id) <> 1
        THEN 'settles a pending transfer that is settled already'
      WHEN a.kind = 'reversal' AND NOT EXISTS (SELECT 1 FROM {s}.{transfers} o
          WHERE o.tenant_id = a.tenant_id AND o.ledger_id = a.ledger_id AND o.id = a.reverses
            AND o.status = 'posted' AND o.amount = a.amount AND o.currency = a.currency AND o.reversed_by = a.id)
        THEN 'does not reverse a posted transfer through the reverse function'
      WHEN a.kind = 'reversal' AND (SELECT count(*) FROM {s}.{transfers} o WHERE o.tenant_id = a.tenant_id
          AND o.ledger_id = a.ledger_id AND o.reverses = a.reverses) <> 1
        THEN 'reverses a transfer that is reversed already'
    END AS why
    FROM added a
  ) x WHERE x.why IS NOT NULL LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION 'ledger: the transfer % %', bad.id, bad.why USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END
$$;
CREATE TRIGGER {transfers}_balance AFTER INSERT ON {s}.{transfers} REFERENCING NEW TABLE AS added
  FOR EACH STATEMENT EXECUTE FUNCTION {s}.ledger_check_transfers();
CREATE FUNCTION {s}.ledger_check_change() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  IF NEW.status IS DISTINCT FROM OLD.status AND NOT EXISTS (SELECT 1 FROM {s}.{transfers} o
      WHERE o.tenant_id = NEW.tenant_id AND o.ledger_id = NEW.ledger_id AND o.pending_id = NEW.id
        AND o.kind = CASE NEW.status WHEN 'posted' THEN 'post' ELSE 'void' END) THEN
    RAISE EXCEPTION 'ledger: the pending transfer % is % with no new row that names it', NEW.id, NEW.status
      USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.reversed_by IS DISTINCT FROM OLD.reversed_by AND NOT EXISTS (SELECT 1 FROM {s}.{transfers} o
      WHERE o.tenant_id = NEW.tenant_id AND o.ledger_id = NEW.ledger_id AND o.id = NEW.reversed_by
        AND o.reverses = NEW.id AND o.kind = 'reversal') THEN
    RAISE EXCEPTION 'ledger: the transfer % is reversed by %, which is not a reversal that names it', NEW.id, NEW.reversed_by
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER {transfers}_change AFTER UPDATE ON {s}.{transfers} DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION {s}.ledger_check_change();
CREATE FUNCTION {s}.ledger_declared(p_ledger text, OUT currency text, OUT closed_before timestamptz,
  OUT pending_timeout interval, OUT snapshot_every bigint)
  LANGUAGE plpgsql STABLE SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  SELECT d.currency, d.closed_before, d.pending_timeout, d.snapshot_every
    INTO currency, closed_before, pending_timeout, snapshot_every
    FROM (VALUES
    {declared}
    ) AS d(ledger_id, currency, closed_before, pending_timeout, snapshot_every)
    WHERE d.ledger_id = p_ledger;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'ledger: % is not a declared ledger', p_ledger USING ERRCODE = 'invalid_parameter_value';
  END IF;
END
$$;
CREATE FUNCTION {s}.ledger_call(p_tenant text, p_ledger text, OUT currency text, OUT closed_before timestamptz,
  OUT pending_timeout interval, OUT snapshot_every bigint)
  LANGUAGE plpgsql STABLE SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  IF p_tenant IS NULL OR p_tenant = '' OR p_tenant IS DISTINCT FROM current_setting('app.tenant', true) THEN
    RAISE EXCEPTION 'ledger: the tenant % is not the tenant of the transaction', p_tenant
      USING ERRCODE = 'insufficient_privilege';
  END IF;
  IF current_setting('transaction_isolation') NOT IN ('read committed', 'serializable') THEN
    RAISE EXCEPTION 'ledger: a ledger write needs READ COMMITTED or SERIALIZABLE, not %',
      current_setting('transaction_isolation') USING ERRCODE = '{isolation}';
  END IF;
  SELECT d.currency, d.closed_before, d.pending_timeout, d.snapshot_every
    INTO currency, closed_before, pending_timeout, snapshot_every FROM {s}.ledger_declared(p_ledger) d;
END
$$;
CREATE FUNCTION {s}.ledger_text(p_value text, p_what text) RETURNS void LANGUAGE plpgsql IMMUTABLE
  SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  IF p_value IS NULL OR length(p_value) = 0 OR octet_length(p_value) > 255 OR p_value ~ '[[:cntrl:]]' THEN
    RAISE EXCEPTION 'ledger: the % is empty, longer than 255 bytes or holds a control character', p_what
      USING ERRCODE = 'invalid_parameter_value';
  END IF;
END
$$;
CREATE FUNCTION {s}.ledger_key(p_tenant text, p_key text) RETURNS text LANGUAGE sql IMMUTABLE
  SET search_path = pg_catalog, pg_temp
  RETURN encode(sha256(convert_to(p_tenant, 'UTF8') || '\x00'::bytea || convert_to(p_key, 'UTF8')), 'hex');
CREATE FUNCTION {s}.ledger_fingerprint(p_kind text, p_code text, p_entries jsonb, p_effective timestamptz,
  p_pending uuid, p_reverses uuid, p_amount bigint) RETURNS text LANGUAGE sql IMMUTABLE
  SET search_path = pg_catalog, pg_temp
  RETURN encode(sha256(convert_to(jsonb_build_array(p_kind, p_code, p_entries,
    extract(epoch FROM p_effective) * 1000000, p_pending::text, p_reverses::text, p_amount)::text, 'UTF8')), 'hex');
CREATE FUNCTION {s}.ledger_replay(p_ledger text, p_key text, p_fingerprint text) RETURNS uuid LANGUAGE plpgsql
  SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  found_id uuid;
  found_fingerprint text;
BEGIN
  SELECT t.id, t.fingerprint INTO found_id, found_fingerprint FROM {s}.{transfers} t
    WHERE t.ledger_id = p_ledger AND t.idempotency_key = p_key AND t.tenant_id = current_setting('app.tenant', true);
  IF NOT FOUND THEN
    RETURN NULL;
  END IF;
  IF found_fingerprint <> p_fingerprint THEN
    RAISE EXCEPTION 'ledger: the idempotency key names the transfer % of another body', found_id USING ERRCODE = '{reused}';
  END IF;
  RETURN found_id;
END
$$;
CREATE FUNCTION {s}.ledger_entries(p_tenant text, p_ledger text, p_currency text, p_entries jsonb) RETURNS bigint
  LANGUAGE plpgsql STABLE SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  debits numeric;
  credits numeric;
  bad text;
BEGIN
  IF p_entries IS NULL OR jsonb_typeof(p_entries) <> 'array' OR jsonb_array_length(p_entries) < 2 THEN
    RAISE EXCEPTION 'ledger: a transfer has at least two entries' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF EXISTS (SELECT 1 FROM jsonb_array_elements(p_entries) x
      WHERE jsonb_typeof(x) <> 'object' OR jsonb_typeof(x->'account') IS DISTINCT FROM 'string'
        OR (x->>'side') IS NULL OR (x->>'side') NOT IN ('debit', 'credit')
        OR jsonb_typeof(x->'amount') IS DISTINCT FROM 'number' OR (x->>'amount') !~ '^[1-9][0-9]{0,18}$') THEN
    RAISE EXCEPTION 'ledger: each entry needs an account, a side of debit or credit and a positive int64 amount'
      USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF EXISTS (SELECT 1 FROM jsonb_array_elements(p_entries) x WHERE (x->>'amount')::numeric > 9223372036854775807) THEN
    RAISE EXCEPTION 'ledger: an entry amount overflows int64 minor units' USING ERRCODE = 'numeric_value_out_of_range';
  END IF;
  SELECT coalesce(sum((x->>'amount')::numeric) FILTER (WHERE x->>'side' = 'debit'), 0),
         coalesce(sum((x->>'amount')::numeric) FILTER (WHERE x->>'side' = 'credit'), 0)
    INTO debits, credits FROM jsonb_array_elements(p_entries) x;
  IF debits <> credits THEN
    RAISE EXCEPTION 'ledger: the debits % differ from the credits %, so the transfer does not balance', debits, credits
      USING ERRCODE = 'check_violation';
  END IF;
  IF debits > 9223372036854775807 THEN
    RAISE EXCEPTION 'ledger: the amount % overflows int64 minor units', debits USING ERRCODE = 'numeric_value_out_of_range';
  END IF;
  SELECT x->>'account' INTO bad FROM jsonb_array_elements(p_entries) x LEFT JOIN {s}.{accounts} c
    ON c.tenant_id = p_tenant AND c.ledger_id = p_ledger AND c.id = x->>'account'
    WHERE c.id IS NULL OR c.currency <> p_currency LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION 'ledger: the entry account % is not an account of the ledger % in %', bad, p_ledger, p_currency
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN debits::bigint;
END
$$;
CREATE FUNCTION {s}.ledger_balance(p_ledger text, p_account text) RETURNS numeric LANGUAGE sql STABLE
  SET search_path = pg_catalog, pg_temp
  RETURN (SELECT coalesce(s.amount, 0)::numeric + coalesce((SELECT sum(` + signedSQL + `) FROM {s}.{entries} e
      WHERE e.tenant_id = a.tenant_id AND e.ledger_id = a.ledger_id AND e.account_id = a.id AND NOT e.pending
        AND (s.through_xid IS NULL OR e.txid >= s.through_xid)), 0)
    FROM {s}.{accounts} a LEFT JOIN LATERAL (SELECT b.amount, b.through_xid FROM {s}.{snapshots} b
      WHERE b.tenant_id = a.tenant_id AND b.ledger_id = a.ledger_id AND b.account_id = a.id
      ORDER BY b.through_xid DESC LIMIT 1) s ON true
    WHERE a.tenant_id = current_setting('app.tenant', true) AND a.ledger_id = p_ledger AND a.id = p_account);
CREATE FUNCTION {s}.ledger_holds(p_ledger text, p_account text) RETURNS numeric LANGUAGE sql STABLE
  SET search_path = pg_catalog, pg_temp
  RETURN (SELECT coalesce(sum(LEAST(` + signedSQL + `, 0)), 0) FROM {s}.{entries} e
    JOIN {s}.{accounts} a ON a.tenant_id = e.tenant_id AND a.ledger_id = e.ledger_id AND a.id = e.account_id
    JOIN {s}.{transfers} p ON p.tenant_id = e.tenant_id AND p.ledger_id = e.ledger_id AND p.id = e.transfer_id
    WHERE e.tenant_id = current_setting('app.tenant', true) AND e.ledger_id = p_ledger AND e.account_id = p_account
      AND e.pending AND p.status = 'pending' AND p.expires_at > now());
CREATE FUNCTION {s}.ledger_lock_limited(p_tenant text, p_ledger text, p_entries jsonb) RETURNS void LANGUAGE plpgsql
  SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  PERFORM 1 FROM {s}.{accounts} c
    WHERE c.tenant_id = p_tenant AND c.ledger_id = p_ledger AND c.min_balance IS NOT NULL
      AND c.id IN (SELECT x->>'account' FROM jsonb_array_elements(p_entries) x)
    ORDER BY c.id FOR UPDATE;
END
$$;
CREATE FUNCTION {s}.ledger_limit(p_tenant text, p_ledger text, p_entries jsonb, p_pending boolean) RETURNS void
  LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  acc record;
  available numeric;
  delta numeric;
BEGIN
  FOR acc IN SELECT c.id, c.normal, c.min_balance FROM {s}.{accounts} c
      WHERE c.tenant_id = p_tenant AND c.ledger_id = p_ledger AND c.min_balance IS NOT NULL
        AND c.id IN (SELECT x->>'account' FROM jsonb_array_elements(p_entries) x)
      ORDER BY c.id LOOP
    available := {s}.ledger_balance(p_ledger, acc.id) + {s}.ledger_holds(p_ledger, acc.id);
    SELECT coalesce(sum(CASE WHEN x->>'side' = acc.normal THEN (x->>'amount')::numeric ELSE -(x->>'amount')::numeric END)
        FILTER (WHERE NOT p_pending OR x->>'side' <> acc.normal), 0)
      INTO delta FROM jsonb_array_elements(p_entries) x WHERE x->>'account' = acc.id;
    IF available + delta < acc.min_balance THEN
      RAISE EXCEPTION 'ledger: the account % would have %, below its minimum %', acc.id, available + delta, acc.min_balance
        USING ERRCODE = '{belowmin}';
    END IF;
  END LOOP;
END
$$;
CREATE FUNCTION {s}.ledger_insert(p_tenant text, p_ledger text, p_id uuid, p_key text, p_fingerprint text, p_kind text,
  p_code text, p_amount bigint, p_currency text, p_status text, p_pending uuid, p_reverses uuid, p_expires timestamptz,
  p_effective timestamptz, p_entries jsonb) RETURNS uuid LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  new_id uuid;
BEGIN
  WITH t AS (
    INSERT INTO {s}.{transfers} (tenant_id, ledger_id, id, idempotency_key, fingerprint, kind, code, amount, currency,
      status, pending_id, reverses, expires_at, effective_at)
    VALUES (p_tenant, p_ledger, coalesce(p_id, gen_random_uuid()), p_key, p_fingerprint, p_kind, p_code, p_amount,
      p_currency, p_status, p_pending, p_reverses, p_expires, coalesce(p_effective, now()))
    ON CONFLICT (ledger_id, idempotency_key) DO NOTHING RETURNING id, effective_at, posted_at
  ), e AS (
    INSERT INTO {s}.{entries} (tenant_id, ledger_id, transfer_id, seq, account_id, side, amount, currency, pending,
      effective_at, posted_at)
    SELECT p_tenant, p_ledger, t.id, x.seq, x.account, x.side, x.amount, p_currency, p_kind = 'pending',
      t.effective_at, t.posted_at
    FROM t, ROWS FROM (jsonb_to_recordset(p_entries) AS (account text, side text, amount bigint))
      WITH ORDINALITY AS x(account, side, amount, seq)
    RETURNING 1
  )
  SELECT t.id INTO new_id FROM t WHERE (SELECT count(*) FROM e) >= 0;
  RETURN new_id;
END
$$;
CREATE FUNCTION {s}.ledger_write(p_kind text, p_tenant text, p_ledger text, p_key text, p_code text, p_entries jsonb,
  p_effective timestamptz) RETURNS TABLE (out_id uuid, out_replayed boolean) LANGUAGE plpgsql
  SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  d record;
  total bigint;
  stored_key text;
  fp text;
  done uuid;
BEGIN
  SELECT * INTO d FROM {s}.ledger_call(p_tenant, p_ledger);
  PERFORM {s}.ledger_text(p_key, 'idempotency key');
  PERFORM {s}.ledger_text(p_code, 'transfer code');
  IF p_effective IS NOT NULL AND p_effective > now() THEN
    RAISE EXCEPTION 'ledger: the effective time % is in the future', p_effective USING ERRCODE = '{effective}';
  END IF;
  IF coalesce(p_effective, now()) < d.closed_before THEN
    RAISE EXCEPTION 'ledger: the effective time % is before the close date % of the ledger %',
      coalesce(p_effective, now()), d.closed_before, p_ledger USING ERRCODE = '{effective}';
  END IF;
  IF p_kind = 'pending' AND d.pending_timeout <= interval '0' THEN
    RAISE EXCEPTION 'ledger: the ledger % declares no two-phase transfers', p_ledger USING ERRCODE = 'invalid_parameter_value';
  END IF;
  total := {s}.ledger_entries(p_tenant, p_ledger, d.currency, p_entries);
  PERFORM {s}.ledger_lock_limited(p_tenant, p_ledger, p_entries);
  stored_key := {s}.ledger_key(p_tenant, p_key);
  fp := {s}.ledger_fingerprint(p_kind, p_code, p_entries, p_effective, NULL, NULL, total);
  done := {s}.ledger_replay(p_ledger, stored_key, fp);
  IF done IS NOT NULL THEN
    RETURN QUERY SELECT done, true;
    RETURN;
  END IF;
  PERFORM {s}.ledger_limit(p_tenant, p_ledger, p_entries, p_kind = 'pending');
  done := {s}.ledger_insert(p_tenant, p_ledger, NULL, stored_key, fp, p_kind, p_code, total, d.currency,
    CASE p_kind WHEN 'pending' THEN 'pending' ELSE 'posted' END, NULL, NULL,
    CASE p_kind WHEN 'pending' THEN now() + d.pending_timeout END, p_effective, p_entries);
  IF done IS NULL THEN
    RETURN QUERY SELECT {s}.ledger_replay(p_ledger, stored_key, fp), true;
    RETURN;
  END IF;
  RETURN QUERY SELECT done, false;
END
$$;
CREATE FUNCTION {s}.ledger_post(p_tenant text, p_ledger text, p_key text, p_code text, p_entries jsonb,
  p_effective timestamptz) RETURNS TABLE (out_id uuid, out_replayed boolean) LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  RETURN QUERY SELECT * FROM {s}.ledger_write('transfer', p_tenant, p_ledger, p_key, p_code, p_entries, p_effective);
END
$$;
CREATE FUNCTION {s}.ledger_pend(p_tenant text, p_ledger text, p_key text, p_code text, p_entries jsonb,
  p_effective timestamptz) RETURNS TABLE (out_id uuid, out_replayed boolean) LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  RETURN QUERY SELECT * FROM {s}.ledger_write('pending', p_tenant, p_ledger, p_key, p_code, p_entries, p_effective);
END
$$;
CREATE FUNCTION {s}.ledger_settle(p_tenant text, p_ledger text, p_key text, p_pending uuid, p_entries jsonb,
  p_to text) RETURNS TABLE (out_id uuid, out_replayed boolean) LANGUAGE plpgsql
  SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  d record;
  p record;
  total bigint;
  stored_key text;
  fp text;
  done uuid;
  prior text := current_setting('{change}', true);
BEGIN
  SELECT * INTO d FROM {s}.ledger_call(p_tenant, p_ledger);
  PERFORM {s}.ledger_text(p_key, 'idempotency key');
  IF p_pending IS NULL THEN
    RAISE EXCEPTION 'ledger: no pending transfer is named' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  SELECT t.* INTO p FROM {s}.{transfers} t
    WHERE t.tenant_id = p_tenant AND t.ledger_id = p_ledger AND t.id = p_pending AND t.kind = 'pending' FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'ledger: % is not a pending transfer of the tenant', p_pending USING ERRCODE = '{nopending}';
  END IF;
  IF p_to = 'posted' THEN
    total := {s}.ledger_entries(p_tenant, p_ledger, d.currency, p_entries);
  ELSE
    total := p.amount;
    p_entries := '[]'::jsonb;
  END IF;
  stored_key := {s}.ledger_key(p_tenant, p_key);
  fp := {s}.ledger_fingerprint(CASE p_to WHEN 'posted' THEN 'post' ELSE 'void' END, p.code, p_entries, NULL,
    p_pending, NULL, total);
  done := {s}.ledger_replay(p_ledger, stored_key, fp);
  IF done IS NOT NULL THEN
    RETURN QUERY SELECT done, true;
    RETURN;
  END IF;
  IF p.status = 'posted' THEN
    RAISE EXCEPTION 'ledger: the pending transfer % is posted already', p_pending USING ERRCODE = '{posted}';
  END IF;
  IF p.status = 'voided' THEN
    RAISE EXCEPTION 'ledger: the pending transfer % is voided already', p_pending USING ERRCODE = '{voided}';
  END IF;
  IF p_to = 'posted' THEN
    IF p.expires_at <= now() THEN
      RAISE EXCEPTION 'ledger: the pending transfer % expired at %', p_pending, p.expires_at USING ERRCODE = '{expired}';
    END IF;
    IF total > p.amount THEN
      RAISE EXCEPTION 'ledger: the amount % is larger than the pending amount %', total, p.amount
        USING ERRCODE = '{exceeds}';
    END IF;
    IF EXISTS (SELECT 1 FROM (SELECT x->>'account' AS account_id, x->>'side' AS side,
            sum((x->>'amount')::numeric) AS amount
          FROM jsonb_array_elements(p_entries) x GROUP BY 1, 2) n
        LEFT JOIN (SELECT e.account_id, e.side, sum(e.amount) AS amount FROM {s}.{entries} e
          WHERE e.tenant_id = p_tenant AND e.ledger_id = p_ledger AND e.transfer_id = p_pending GROUP BY 1, 2) o
          ON o.account_id = n.account_id AND o.side = n.side
        WHERE o.account_id IS NULL OR n.amount > o.amount) THEN
      RAISE EXCEPTION 'ledger: the entries are not a split, per side, of the pending entries of %', p_pending
        USING ERRCODE = '{notsplit}';
    END IF;
  END IF;
  PERFORM set_config('{change}', 'settle:' || p_pending::text, true);
  UPDATE {s}.{transfers} SET status = p_to WHERE tenant_id = p_tenant AND ledger_id = p_ledger AND id = p_pending;
  PERFORM set_config('{change}', coalesce(prior, ''), true);
  done := {s}.ledger_insert(p_tenant, p_ledger, NULL, stored_key, fp, CASE p_to WHEN 'posted' THEN 'post' ELSE 'void' END,
    p.code, total, d.currency, p_to, p_pending, NULL, NULL, NULL, p_entries);
  IF done IS NULL THEN
    RAISE EXCEPTION 'ledger: the idempotency key is taken by a concurrent call' USING ERRCODE = '{reused}';
  END IF;
  RETURN QUERY SELECT done, false;
END
$$;
CREATE FUNCTION {s}.ledger_post_pending(p_tenant text, p_ledger text, p_key text, p_pending uuid, p_entries jsonb)
  RETURNS TABLE (out_id uuid, out_replayed boolean) LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  RETURN QUERY SELECT * FROM {s}.ledger_settle(p_tenant, p_ledger, p_key, p_pending, p_entries, 'posted');
END
$$;
CREATE FUNCTION {s}.ledger_void(p_tenant text, p_ledger text, p_key text, p_pending uuid)
  RETURNS TABLE (out_id uuid, out_replayed boolean) LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  RETURN QUERY SELECT * FROM {s}.ledger_settle(p_tenant, p_ledger, p_key, p_pending, NULL, 'voided');
END
$$;
CREATE FUNCTION {s}.ledger_reverse(p_tenant text, p_ledger text, p_key text, p_original uuid, p_entries jsonb)
  RETURNS TABLE (out_id uuid, out_replayed boolean) LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  d record;
  o record;
  total bigint;
  stored_key text;
  fp text;
  done uuid;
  new_id uuid := gen_random_uuid();
  prior text := current_setting('{change}', true);
BEGIN
  SELECT * INTO d FROM {s}.ledger_call(p_tenant, p_ledger);
  PERFORM {s}.ledger_text(p_key, 'idempotency key');
  IF p_original IS NULL THEN
    RAISE EXCEPTION 'ledger: no transfer is named' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  SELECT t.* INTO o FROM {s}.{transfers} t
    WHERE t.tenant_id = p_tenant AND t.ledger_id = p_ledger AND t.id = p_original FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'ledger: % is not a posted transfer of the tenant', p_original USING ERRCODE = '{notposted}';
  END IF;
  total := {s}.ledger_entries(p_tenant, p_ledger, d.currency, p_entries);
  stored_key := {s}.ledger_key(p_tenant, p_key);
  fp := {s}.ledger_fingerprint('reversal', o.code, p_entries, NULL, NULL, p_original, total);
  done := {s}.ledger_replay(p_ledger, stored_key, fp);
  IF done IS NOT NULL THEN
    RETURN QUERY SELECT done, true;
    RETURN;
  END IF;
  IF o.status <> 'posted' OR o.kind NOT IN ('transfer', 'post', 'reversal') THEN
    RAISE EXCEPTION 'ledger: % is not a posted transfer of the tenant', p_original USING ERRCODE = '{notposted}';
  END IF;
  IF o.reversed_by IS NOT NULL THEN
    RAISE EXCEPTION 'ledger: the transfer % is reversed already', p_original USING ERRCODE = '{reversed}';
  END IF;
  IF (SELECT jsonb_agg(jsonb_build_object('account', e.account_id,
          'side', CASE e.side WHEN 'debit' THEN 'credit' ELSE 'debit' END, 'amount', e.amount) ORDER BY e.seq)
        FROM {s}.{entries} e WHERE e.tenant_id = p_tenant AND e.ledger_id = p_ledger AND e.transfer_id = p_original)
      IS DISTINCT FROM (SELECT jsonb_agg(jsonb_build_object('account', r.x->>'account', 'side', r.x->>'side',
          'amount', (r.x->>'amount')::bigint) ORDER BY r.n) FROM jsonb_array_elements(p_entries) WITH ORDINALITY AS r(x, n)) THEN
    RAISE EXCEPTION 'ledger: the entries are not the entries of % with each side swapped', p_original
      USING ERRCODE = '{notmirror}';
  END IF;
  PERFORM {s}.ledger_lock_limited(p_tenant, p_ledger, p_entries);
  PERFORM {s}.ledger_limit(p_tenant, p_ledger, p_entries, false);
  PERFORM set_config('{change}', 'reverse:' || p_original::text, true);
  UPDATE {s}.{transfers} SET reversed_by = new_id WHERE tenant_id = p_tenant AND ledger_id = p_ledger AND id = p_original;
  PERFORM set_config('{change}', coalesce(prior, ''), true);
  done := {s}.ledger_insert(p_tenant, p_ledger, new_id, stored_key, fp, 'reversal', o.code, total, d.currency, 'posted',
    NULL, p_original, NULL, NULL, p_entries);
  IF done IS NULL THEN
    RAISE EXCEPTION 'ledger: the idempotency key is taken by a concurrent call' USING ERRCODE = '{reused}';
  END IF;
  RETURN QUERY SELECT done, false;
END
$$;
CREATE FUNCTION {s}.ledger_open(p_tenant text, p_ledger text, p_id text, p_code text, p_normal text, p_currency text,
  p_min_balance bigint, p_hot boolean) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  d record;
BEGIN
  SELECT * INTO d FROM {s}.ledger_call(p_tenant, p_ledger);
  PERFORM {s}.ledger_text(p_id, 'account ID');
  PERFORM {s}.ledger_text(p_code, 'account code');
  IF p_normal IS NULL OR p_normal NOT IN ('debit', 'credit') THEN
    RAISE EXCEPTION 'ledger: the normal side % is not debit or credit', p_normal USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF p_currency IS DISTINCT FROM d.currency THEN
    RAISE EXCEPTION 'ledger: the account is in %, and the ledger % holds only %', p_currency, p_ledger, d.currency
      USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF p_hot IS NULL OR (p_hot AND p_min_balance IS NOT NULL) THEN
    RAISE EXCEPTION 'ledger: a hot account has no balance limit' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  INSERT INTO {s}.{accounts} (tenant_id, ledger_id, id, code, normal, currency, min_balance, hot)
    VALUES (p_tenant, p_ledger, p_id, p_code, p_normal, p_currency, p_min_balance, p_hot);
END
$$;
CREATE FUNCTION {s}.ledger_snapshot(p_tenant text, p_ledger text, p_token bigint) RETURNS integer LANGUAGE plpgsql
  SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  d record;
  written integer;
BEGIN
  SELECT * INTO d FROM {s}.ledger_call(p_tenant, p_ledger);
  IF p_token IS NULL OR p_token < 1 THEN
    RAISE EXCEPTION 'ledger: a snapshot needs the fencing token of the lease' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  -- A snapshot of a limited account waits for a post that holds the account's row lock, in account ID order.
  PERFORM 1 FROM {s}.{accounts} c WHERE c.tenant_id = p_tenant AND c.ledger_id = p_ledger AND c.min_balance IS NOT NULL
    ORDER BY c.id FOR SHARE;
  WITH x AS (SELECT pg_snapshot_xmin(pg_current_snapshot()) AS xmin),
  due AS (
    SELECT a.id, s.amount AS base, s.entry_count AS base_count, sum(` + signedSQL + `) AS delta, count(*) AS n
    FROM {s}.{accounts} a
    LEFT JOIN LATERAL (SELECT b.amount, b.through_xid, b.entry_count FROM {s}.{snapshots} b
      WHERE b.tenant_id = a.tenant_id AND b.ledger_id = a.ledger_id AND b.account_id = a.id
      ORDER BY b.through_xid DESC LIMIT 1) s ON true
    JOIN {s}.{entries} e ON e.tenant_id = a.tenant_id AND e.ledger_id = a.ledger_id AND e.account_id = a.id
      AND NOT e.pending AND (s.through_xid IS NULL OR e.txid >= s.through_xid) AND e.txid < (SELECT xmin FROM x)
    WHERE a.tenant_id = p_tenant AND a.ledger_id = p_ledger
    GROUP BY a.id, s.amount, s.entry_count
    HAVING count(*) >= d.snapshot_every
  )
  INSERT INTO {s}.{snapshots} (tenant_id, ledger_id, account_id, through_xid, amount, entry_count, fence_token)
  SELECT p_tenant, p_ledger, due.id, (SELECT xmin FROM x), coalesce(due.base, 0) + due.delta,
    coalesce(due.base_count, 0) + due.n, p_token
  FROM due;
  GET DIAGNOSTICS written = ROW_COUNT;
  RETURN written;
END
$$;
ALTER TABLE {s}.{accounts} ENABLE ROW LEVEL SECURITY;
ALTER TABLE {s}.{accounts} FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_wall ON {s}.{accounts}
  USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true));
ALTER TABLE {s}.{transfers} ENABLE ROW LEVEL SECURITY;
ALTER TABLE {s}.{transfers} FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_wall ON {s}.{transfers}
  USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true));
ALTER TABLE {s}.{entries} ENABLE ROW LEVEL SECURITY;
ALTER TABLE {s}.{entries} FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_wall ON {s}.{entries}
  USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true));
ALTER TABLE {s}.{snapshots} ENABLE ROW LEVEL SECURITY;
ALTER TABLE {s}.{snapshots} FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_wall ON {s}.{snapshots}
  USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true));
REVOKE ALL ON {s}.{accounts}, {s}.{transfers}, {s}.{entries}, {s}.{snapshots} FROM PUBLIC;
REVOKE ALL ON FUNCTION {grants}, {inner}, {s}.ledger_balance(text, text) FROM PUBLIC;
GRANT USAGE ON SCHEMA {s} TO {role};
GRANT SELECT ON {s}.{accounts}, {s}.{transfers}, {s}.{entries}, {s}.{snapshots} TO {role};
GRANT EXECUTE ON FUNCTION {grants}, {s}.ledger_balance(text, text) TO {role};
`

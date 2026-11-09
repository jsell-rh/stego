package ledger

import (
	"errors"
	"regexp"
	"strings"
)

// The tables and the functions of the ledger. Every declared ledger of a service shares the four tables, and each
// row carries its ledger_id. Each table is tenant-owned (ST-STD-TXN-R01).
const (
	// AccountsTable holds one row for each account: its normal side, its currency, and its balance limit or hot
	// flag.
	AccountsTable = "accounts"
	// TransfersTable holds one row for each transfer, pending transfer, post or void of a pending transfer, and
	// reversal.
	TransfersTable = "transfers"
	// EntriesTable holds the debit and credit entries of each transfer.
	EntriesTable = "entries"
	// SnapshotsTable holds the balance snapshots that the snapshot job writes.
	SnapshotsTable = "balance_snapshots"
	// SettleFunction is the owner function that changes the status of a pending transfer, once, from pending
	// to posted or voided. It is the post and void function of ST-SUP-RP08-LC09.
	SettleFunction = "ledger_settle"
	// ReverseFunction is the owner function that sets reversed_by on a posted transfer, once.
	ReverseFunction = "ledger_reverse"
	// LockFunction is the owner function that reads the accounts of a transfer and takes the row lock of each
	// account with a balance limit.
	LockFunction = "ledger_lock"
	// GuardSetting is the transaction-local setting that the two owner functions set, while their one UPDATE
	// runs, to the change that the guard trigger lets through.
	GuardSetting = "stego.ledger_change"
)

// SQLSTATE codes that the owner functions raise. They are the class LG, which PostgreSQL does not use.
const (
	codeAlreadyPosted   = "LG001"
	codeAlreadyVoided   = "LG002"
	codeAlreadyReversed = "LG003"
	codeNotPosted       = "LG004"
	codeNoPending       = "LG005"
)

var plainName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Config is what the DDL text needs. The table owner runs the text in the migration job, as the owner role, so the
// tables and the functions belong to that role (ST-SUP-RP04-LC06).
type Config struct {
	// Schema holds the ledger tables; the empty schema is public.
	Schema string
	// RuntimeRole is the role of the running service.
	RuntimeRole string
}

// DDL returns the statements that create the ledger (ST-SUP-RP08-LC09, ST-STD-TXN-R13): the tables accounts,
// transfers, entries and balance_snapshots; two statement triggers that refuse a set of entries that does not
// balance or is not inserted in the statement of its transfer; the guard trigger, BEFORE UPDATE OR DELETE on
// transfers and entries, which raises on every change except the one change of status of a pending transfer that
// the post and void function makes and the one reversed_by that the reverse function sets, each of which a
// constraint trigger checks at commit against the new row that names the changed one; row-level security
// enabled and forced on the four tables with one policy on the transaction-local tenant setting; the owner
// functions, SECURITY DEFINER with a fixed search_path; and the grants: SELECT and INSERT only on the four tables,
// and EXECUTE on the owner functions, for the runtime role.
//
// The generated migration adds the constraints that vary with the declaration (ST-STD-RUNTIME-R05): the CHECK
// constraints of the declared transfer codes, account limits and ledger currencies.
func DDL(c Config) (string, error) {
	if c.Schema == "" {
		c.Schema = "public"
	}
	if !plainName.MatchString(c.Schema) || !plainName.MatchString(c.RuntimeRole) {
		return "", errors.New("ledger: the schema and the runtime role must be plain lower-case names")
	}
	return strings.NewReplacer(
		"{s}", c.Schema,
		"{role}", c.RuntimeRole,
		"{accounts}", AccountsTable,
		"{transfers}", TransfersTable,
		"{entries}", EntriesTable,
		"{snapshots}", SnapshotsTable,
		"{settle}", SettleFunction,
		"{reverse}", ReverseFunction,
		"{lock}", LockFunction,
		"{change}", GuardSetting,
		"{posted}", codeAlreadyPosted,
		"{voided}", codeAlreadyVoided,
		"{reversed}", codeAlreadyReversed,
		"{notposted}", codeNotPosted,
		"{nopending}", codeNoPending,
	).Replace(ddlText), nil
}

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
CREATE TABLE {s}.{snapshots} (
  tenant_id text NOT NULL,
  ledger_id text NOT NULL,
  account_id text NOT NULL,
  through_xid xid8 NOT NULL,
  amount bigint NOT NULL,
  entry_count bigint NOT NULL,
  fence_token bigint NOT NULL,
  taken_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, ledger_id, account_id, through_xid),
  FOREIGN KEY (tenant_id, ledger_id, account_id) REFERENCES {s}.{accounts} (tenant_id, ledger_id, id)
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
        THEN 'does not settle a pending transfer through the post and void function'
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
CREATE FUNCTION {s}.{settle}(p_pending uuid, p_status text) RETURNS text LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  prior text := current_setting('{change}', true);
  was text;
BEGIN
  IF p_status IS NULL OR p_status NOT IN ('posted', 'voided') THEN
    RAISE EXCEPTION 'ledger: a pending transfer is posted or voided, not %', p_status
      USING ERRCODE = 'invalid_parameter_value';
  END IF;
  SELECT t.status INTO was FROM {s}.{transfers} t
    WHERE t.id = p_pending AND t.kind = 'pending' AND t.tenant_id = current_setting('app.tenant', true)
    FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'ledger: % is not a pending transfer of the tenant', p_pending USING ERRCODE = '{nopending}';
  END IF;
  IF was = 'posted' THEN
    RAISE EXCEPTION 'ledger: the pending transfer % is posted already', p_pending USING ERRCODE = '{posted}';
  END IF;
  IF was = 'voided' THEN
    RAISE EXCEPTION 'ledger: the pending transfer % is voided already', p_pending USING ERRCODE = '{voided}';
  END IF;
  PERFORM set_config('{change}', 'settle:' || p_pending::text, true);
  UPDATE {s}.{transfers} SET status = p_status
    WHERE id = p_pending AND tenant_id = current_setting('app.tenant', true);
  PERFORM set_config('{change}', coalesce(prior, ''), true);
  RETURN was;
END
$$;
CREATE FUNCTION {s}.{reverse}(p_original uuid, p_reversal uuid) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  prior text := current_setting('{change}', true);
  orig record;
BEGIN
  SELECT t.status, t.kind, t.reversed_by INTO orig FROM {s}.{transfers} t
    WHERE t.id = p_original AND t.tenant_id = current_setting('app.tenant', true)
    FOR UPDATE;
  IF NOT FOUND OR orig.status <> 'posted' OR orig.kind NOT IN ('transfer', 'post', 'reversal') THEN
    RAISE EXCEPTION 'ledger: % is not a posted transfer of the tenant', p_original USING ERRCODE = '{notposted}';
  END IF;
  IF orig.reversed_by IS NOT NULL THEN
    RAISE EXCEPTION 'ledger: the transfer % is reversed already', p_original USING ERRCODE = '{reversed}';
  END IF;
  PERFORM set_config('{change}', 'reverse:' || p_original::text, true);
  UPDATE {s}.{transfers} SET reversed_by = p_reversal
    WHERE id = p_original AND tenant_id = current_setting('app.tenant', true);
  PERFORM set_config('{change}', coalesce(prior, ''), true);
END
$$;
CREATE FUNCTION {s}.{lock}(p_ledger text, p_accounts jsonb)
  RETURNS TABLE (id text, normal text, currency text, min_balance bigint, hot boolean)
  LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  PERFORM 1 FROM {s}.{accounts} a
    WHERE a.tenant_id = current_setting('app.tenant', true) AND a.ledger_id = p_ledger
      AND a.id IN (SELECT jsonb_array_elements_text(p_accounts)) AND a.min_balance IS NOT NULL
    ORDER BY a.id FOR UPDATE;
  RETURN QUERY SELECT a.id, a.normal, a.currency::text, a.min_balance, a.hot FROM {s}.{accounts} a
    WHERE a.tenant_id = current_setting('app.tenant', true) AND a.ledger_id = p_ledger
      AND a.id IN (SELECT jsonb_array_elements_text(p_accounts))
    ORDER BY a.id;
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
REVOKE ALL ON FUNCTION {s}.ledger_guard(), {s}.ledger_check_entries(), {s}.ledger_check_transfers(), {s}.ledger_check_change(),
  {s}.{settle}(uuid, text), {s}.{reverse}(uuid, uuid), {s}.{lock}(text, jsonb) FROM PUBLIC;
GRANT USAGE ON SCHEMA {s} TO {role};
GRANT SELECT, INSERT ON {s}.{accounts}, {s}.{transfers}, {s}.{entries}, {s}.{snapshots} TO {role};
GRANT EXECUTE ON FUNCTION {s}.{settle}(uuid, text), {s}.{reverse}(uuid, uuid), {s}.{lock}(text, jsonb) TO {role};
`

package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
)

// HashSize is the size of a hash and of a prev_hash: SHA-256.
const HashSize = sha256.Size

// ZeroHash returns the prev_hash of the first row of a chain: 32 zero bytes (ST-SUP-RP05-LC04).
func ZeroHash() []byte {
	return make([]byte, HashSize)
}

// Hash returns SHA-256 over the canonical redacted record of the event (CanonicalRecord) followed by its
// prev_hash (ST-SUP-RP05-LC04).
func Hash(e Event) ([]byte, error) {
	rec, err := CanonicalRecord(e)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write(rec)
	h.Write(e.PrevHash)
	return h.Sum(nil), nil
}

// Link returns the event as the next row after a row whose hash is prev: its prev_hash is prev and its hash is
// computed over it. Give ZeroHash for the first row of a chain.
func Link(e Event, prev []byte) (Event, error) {
	if len(prev) != HashSize {
		return Event{}, fmt.Errorf("audit: a prev_hash has %d bytes, not %d", len(prev), HashSize)
	}
	e.PrevHash = bytes.Clone(prev)
	h, err := Hash(e)
	if err != nil {
		return Event{}, err
	}
	e.Hash = h
	return e, nil
}

// AnchorKind is the kind of an anchor: periodic, from the anchor job, or retention, from a partition drop.
type AnchorKind string

// The two anchor kinds.
const (
	AnchorPeriodic  AnchorKind = "periodic"
	AnchorRetention AnchorKind = "retention"
)

// Anchor is the head of one tenant's chain at one time, as the sink holds it (ST-SUP-RP05-LC08): the hash of the
// row whose seq is Count.
type Anchor struct {
	Tenant string
	Kind   AnchorKind
	Head   []byte
	Count  int64
}

// FindingKind names what the verifier found.
type FindingKind string

// The kinds of finding.
const (
	// FindingSeq is the first row whose seq is not one higher than the row before it, or than the checkpoint.
	FindingSeq FindingKind = "seq_differs"
	// FindingPrevHash is the first row whose prev_hash is not the hash of the row before it.
	FindingPrevHash FindingKind = "prev_hash_differs"
	// FindingHash is the first row whose stored hash is not the hash of its content.
	FindingHash FindingKind = "hash_differs"
	// FindingHead is an anchor whose count names a row with another hash.
	FindingHead FindingKind = "head_differs_from_anchor"
	// FindingTruncated is a chain whose highest seq is lower than the highest anchored count.
	FindingTruncated FindingKind = "truncated"
	// FindingForged is an anchor whose count is lower than an earlier anchor's count, or whose head is not on
	// the chain.
	FindingForged FindingKind = "forged_anchor"
)

// Finding is one difference that the verifier found. Seq and ID name the row of a row finding; Anchored and Found
// are the counts of a truncation; Anchor is the index of the anchor in the list Verify got.
type Finding struct {
	Kind     FindingKind
	Seq      int64
	ID       UUID
	Anchored int64
	Found    int64
	Anchor   int
}

func (f Finding) String() string {
	switch f.Kind {
	case FindingTruncated:
		return fmt.Sprintf("audit chain truncated: anchored to seq %d, the table ends at seq %d", f.Anchored, f.Found)
	case FindingHead, FindingForged:
		return fmt.Sprintf("audit anchor %d: %s at seq %d", f.Anchor, f.Kind, f.Anchored)
	default:
		return fmt.Sprintf("audit chain %s at seq %d (%s)", f.Kind, f.Seq, f.ID)
	}
}

const (
	selectCheckpoint = `SELECT seq, hash FROM ` + CheckpointsTable + ` WHERE tenant_id = $1 ORDER BY seq DESC LIMIT 1`
	selectChain      = `SELECT seq, id::text, occurred_at, recorded_at, tenant_id, actor_id, actor_kind, actor_ip,
 user_agent, action, resource_type, resource_id, outcome, error_code, request_id, trace_id, span_id, idempotency_key,
 before::text, after::text, patch::text, service, service_version, declaration_version, prev_hash, hash
 FROM ` + EventsTable + ` WHERE tenant_id = $1 AND seq > $2 ORDER BY seq`
	selectTenant = `SELECT coalesce(current_setting('app.tenant', true), '')`
)

// Verify walks the chain of one tenant in seq order (ST-SUP-RP05-LC06), in tx, a transaction of that tenant. It
// starts after the chain's latest retention checkpoint (ST-SUP-RP05-LC11), with the checkpoint's hash as the
// first prev_hash, or at the first row with 32 zero bytes. It checks each row as it reads it, and reports the
// first row whose seq, prev_hash or hash differs. Then it checks the periodic anchors that the sink holds for the
// chain, in the order the sink holds them: the row whose seq is an anchor's count must have the anchor's head, the
// highest seq must not be lower than the highest anchored count, and an anchor whose count is lower than an
// earlier one's, or whose row is not on the chain, is forged. It keeps only the previous hash and the anchors, so
// its memory does not grow with the chain. The anchors are values the caller read from the sink: the package has
// no read on the sink (ST-SUP-RP05-LC08).
func Verify(ctx context.Context, tx Tx, tenant string, anchors []Anchor) ([]Finding, error) {
	var set string
	if err := tx.QueryRowContext(ctx, selectTenant).Scan(&set); err != nil {
		return nil, stmtError(ctx, "tenant check", err)
	}
	if tenant == "" || set != tenant {
		return nil, errors.New("audit: Verify runs in a transaction of the tenant it verifies")
	}
	start := int64(0)
	prev := ZeroHash()
	var ckSeq int64
	var ckHash []byte
	switch err := tx.QueryRowContext(ctx, selectCheckpoint, tenant).Scan(&ckSeq, &ckHash); {
	case err == nil:
		start, prev = ckSeq, ckHash
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, stmtError(ctx, "checkpoint read", err)
	}
	// The periodic anchors by the seq they name.
	byCount := map[int64][]int{}
	var maxCount int64
	var findings []Finding
	for i, a := range anchors {
		if a.Kind != AnchorPeriodic || a.Tenant != tenant {
			continue
		}
		if a.Count < maxCount {
			findings = append(findings, Finding{Kind: FindingForged, Anchor: i, Anchored: a.Count})
			continue
		}
		maxCount = a.Count
		byCount[a.Count] = append(byCount[a.Count], i)
	}
	rows, err := tx.QueryContext(ctx, selectChain, tenant, start)
	if err != nil {
		return nil, stmtError(ctx, "chain read", err)
	}
	defer rows.Close()
	expect := start + 1
	broken := false
	highest := start
	for rows.Next() {
		seq, e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		highest = seq
		if !broken {
			switch {
			case seq != expect:
				findings = append(findings, Finding{Kind: FindingSeq, Seq: seq, ID: e.ID})
				broken = true
			case !bytes.Equal(e.PrevHash, prev):
				findings = append(findings, Finding{Kind: FindingPrevHash, Seq: seq, ID: e.ID})
				broken = true
			default:
				h, err := Hash(e)
				if err != nil {
					return nil, err
				}
				if !bytes.Equal(h, e.Hash) {
					findings = append(findings, Finding{Kind: FindingHash, Seq: seq, ID: e.ID})
					broken = true
				}
			}
			prev = e.Hash
			expect = seq + 1
		}
		for _, i := range byCount[seq] {
			if !bytes.Equal(e.Hash, anchors[i].Head) {
				findings = append(findings, Finding{Kind: FindingHead, Seq: seq, ID: e.ID, Anchor: i, Anchored: seq})
			}
			delete(byCount, seq)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, stmtError(ctx, "chain read", err)
	}
	if highest < maxCount {
		findings = append(findings, Finding{Kind: FindingTruncated, Anchored: maxCount, Found: highest})
	}
	for count, idx := range byCount {
		if count <= start || count > highest {
			continue
		}
		for _, i := range idx {
			findings = append(findings, Finding{Kind: FindingForged, Anchor: i, Anchored: count})
		}
	}
	return findings, nil
}

// scanEvent reads one stored row. It holds the stored, redacted states, which no caller can set (ST-SUP-RP05-LC13).
func scanEvent(rows *sql.Rows) (int64, Event, error) {
	var (
		e                                 Event
		seq                               int64
		id                                string
		actorKind, outcome, ip, userAgent string
		before, after, patch              sql.NullString
	)
	if err := rows.Scan(&seq, &id, &e.OccurredAt, &e.RecordedAt, &e.TenantID, &e.ActorID, &actorKind, &ip,
		&userAgent, &e.Action, &e.ResourceType, &e.ResourceID, &outcome, &e.ErrorCode, &e.RequestID, &e.TraceID,
		&e.SpanID, &e.IdempotencyKey, &before, &after, &patch, &e.Service, &e.ServiceVersion,
		&e.DeclarationVersion, &e.PrevHash, &e.Hash); err != nil {
		return 0, Event{}, &StatementError{Step: "chain row read", Code: sqlState(err)}
	}
	u, err := ParseUUID(id)
	if err != nil {
		return 0, Event{}, err
	}
	e.ID = u
	e.ActorKind, e.Outcome = ActorKind(actorKind), Outcome(outcome)
	e.ActorIP, e.UserAgent = Untrusted(ip), Untrusted(userAgent)
	e.Before, e.After, e.Patch = State{nullJSON(before)}, State{nullJSON(after)}, State{nullJSON(patch)}
	return seq, e, nil
}

func nullJSON(s sql.NullString) []byte {
	if !s.Valid {
		return nil
	}
	return []byte(s.String)
}

package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
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

// Head is the head of one service chain: the service and the hash of its last row.
type Head struct {
	Service string
	Hash    []byte
}

// Anchor is a chain head and row count that the anchor job published to the sink (ST-SUP-RP05-LC09).
type Anchor struct {
	Head  Head
	Count int64
}

// FindingKind names what the verifier found.
type FindingKind string

// The kinds of finding.
const (
	// FindingPrevHash is the first row whose prev_hash is not the hash of the row before it.
	FindingPrevHash FindingKind = "prev_hash_differs"
	// FindingHash is the first row whose stored hash is not the hash of its content.
	FindingHash FindingKind = "hash_differs"
	// FindingHead is a chain whose row at the anchored count does not have the anchored hash.
	FindingHead FindingKind = "head_differs_from_anchor"
	// FindingTruncated is a chain with fewer rows than the anchored count.
	FindingTruncated FindingKind = "truncated"
)

// Finding is one difference that the verifier found. Seq and ID name the row, when the finding is about a row;
// Anchored and Found are the counts of a truncation.
type Finding struct {
	Kind     FindingKind
	Seq      int64
	ID       UUID
	Anchored int64
	Found    int64
}

func (f Finding) String() string {
	switch f.Kind {
	case FindingTruncated:
		return fmt.Sprintf("audit chain truncated: the anchor holds %d rows, the table %d", f.Anchored, f.Found)
	case FindingHead:
		return fmt.Sprintf("audit chain head differs from the anchor at row %d (%s)", f.Seq, f.ID)
	default:
		return fmt.Sprintf("audit chain %s at row %d (%s)", f.Kind, f.Seq, f.ID)
	}
}

const selectChain = `SELECT seq, id::text, occurred_at, recorded_at, tenant_id, actor_id, actor_kind, actor_ip, user_agent,
 action, resource_type, resource_id, outcome, error_code, request_id, trace_id, span_id, idempotency_key,
 before::text, after::text, patch::text, service, service_version, declaration_version, prev_hash, hash
 FROM ` + EventsTable + ` WHERE service = $1 ORDER BY seq`

type chainRow struct {
	seq   int64
	event Event
}

// Verify walks the chain of one service in row order (ST-SUP-RP05-LC06). It recomputes every hash and reports
// the first row whose prev_hash or hash differs. Then it compares the chain with the latest anchor that the sink
// holds, when one is given: it reports truncation when the table holds fewer rows than the anchored count, and a
// head that differs when the row at the anchored count does not have the anchored hash. The anchor is a value the
// caller read from the sink: the package has no read on the sink (ST-SUP-RP05-LC08).
func Verify(ctx context.Context, q Tx, service string, latest *Anchor) ([]Finding, error) {
	rows, err := q.Query(ctx, selectChain, service)
	if err != nil {
		return nil, fmt.Errorf("audit: read the chain of %s: %w", service, err)
	}
	var chain []chainRow
	for rows.Next() {
		var (
			r                                 chainRow
			id                                string
			actorKind, outcome, ip, userAgent string
			before, after, patch              *string
		)
		e := &r.event
		if err := rows.Scan(&r.seq, &id, &e.OccurredAt, &e.RecordedAt, &e.TenantID, &e.ActorID, &actorKind, &ip,
			&userAgent, &e.Action, &e.ResourceType, &e.ResourceID, &outcome, &e.ErrorCode, &e.RequestID, &e.TraceID,
			&e.SpanID, &e.IdempotencyKey, &before, &after, &patch, &e.Service, &e.ServiceVersion,
			&e.DeclarationVersion, &e.PrevHash, &e.Hash); err != nil {
			rows.Close()
			return nil, fmt.Errorf("audit: read the chain of %s: %w", service, err)
		}
		u, err := ParseUUID(id)
		if err != nil {
			rows.Close()
			return nil, err
		}
		e.ID = u
		e.ActorKind, e.Outcome = ActorKind(actorKind), Outcome(outcome)
		e.ActorIP, e.UserAgent = Untrusted(ip), Untrusted(userAgent)
		e.Before, e.After, e.Patch = rawState(before), rawState(after), rawState(patch)
		chain = append(chain, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: read the chain of %s: %w", service, err)
	}
	return walk(chain, latest)
}

func rawState(s *string) []byte {
	if s == nil {
		return nil
	}
	return []byte(*s)
}

func walk(chain []chainRow, latest *Anchor) ([]Finding, error) {
	var findings []Finding
	prev := ZeroHash()
	for _, r := range chain {
		if !bytes.Equal(r.event.PrevHash, prev) {
			findings = append(findings, Finding{Kind: FindingPrevHash, Seq: r.seq, ID: r.event.ID})
			break
		}
		h, err := Hash(r.event)
		if err != nil {
			return nil, fmt.Errorf("audit: hash row %d: %w", r.seq, err)
		}
		if !bytes.Equal(h, r.event.Hash) {
			findings = append(findings, Finding{Kind: FindingHash, Seq: r.seq, ID: r.event.ID})
			break
		}
		prev = r.event.Hash
	}
	if latest == nil || latest.Count == 0 {
		return findings, nil
	}
	found := int64(len(chain))
	if found < latest.Count {
		return append(findings, Finding{Kind: FindingTruncated, Anchored: latest.Count, Found: found}), nil
	}
	at := chain[latest.Count-1]
	if !bytes.Equal(at.event.Hash, latest.Head.Hash) {
		findings = append(findings, Finding{Kind: FindingHead, Seq: at.seq, ID: at.event.ID})
	}
	return findings, nil
}

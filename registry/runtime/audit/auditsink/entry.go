package auditsink

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/jsell-rh/stego/registry/runtime/audit"
)

// The kinds of entry that a sink stores or sends.
const (
	KindEvent  = "event"
	KindAnchor = "anchor"
)

// Entry is one audit entry as a sink keeps it: an event or an anchor, with its ID, its hash, the hash before it, its
// tenant and its canonical sink message as the body.
type Entry struct {
	// ID is the event ID, or for an anchor "anchor-" and the SHA-256 of its body in hex, so a resend of one anchor has
	// the same ID.
	ID string
	// Kind is KindEvent or KindAnchor.
	Kind string
	// Tenant is the tenant of the entry's chain.
	Tenant string
	// Hash is the hash of the event, or the head hash of the anchor.
	Hash []byte
	// PrevHash is the hash before the event in its chain; an anchor has none.
	PrevHash []byte
	// Body is the canonical sink message of the entry (audit.EventMessage, audit.AnchorMessage).
	Body []byte
}

// eventEntry gives the entry of an event.
func eventEntry(e audit.Event) (Entry, error) {
	body, err := audit.EventMessage(e)
	if err != nil {
		return Entry{}, err
	}
	if e.TenantID == "" || len(e.Hash) == 0 {
		return Entry{}, errors.New("auditsink: the event has no tenant or no hash")
	}
	return Entry{ID: e.ID.String(), Kind: KindEvent, Tenant: e.TenantID, Hash: e.Hash, PrevHash: e.PrevHash, Body: body}, nil
}

// anchorEntry gives the entry of an anchor.
func anchorEntry(a audit.Anchor) (Entry, error) {
	if a.Tenant == "" || len(a.Head) == 0 {
		return Entry{}, errors.New("auditsink: the anchor has no tenant or no head hash")
	}
	body := audit.AnchorMessage(a)
	sum := sha256.Sum256(body)
	return Entry{ID: "anchor-" + hex.EncodeToString(sum[:]), Kind: KindAnchor, Tenant: a.Tenant, Hash: a.Head, Body: body}, nil
}

// eventEntries gives the entries of events, in their order.
func eventEntries(events []audit.Event) ([]Entry, error) {
	entries := make([]Entry, 0, len(events))
	for _, e := range events {
		entry, err := eventEntry(e)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

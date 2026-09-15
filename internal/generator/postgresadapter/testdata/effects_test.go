package storage

import (
	"context"
	"errors"
	"strings"
	"testing"

	contract "example.com/transaction-test/contracts/storage"
)

func effectWrite(ctx context.Context, s *Store, id, digest string, close bool) (result contract.EffectBinding, err error) {
	err = s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		bindings := tx.(contract.EffectBindingStore)
		var failure error
		if close {
			result, failure = bindings.CloseEffectBinding(ctx, "Record", id, "provider")
		} else {
			result, failure = bindings.BindEffect(ctx, "Record", id, "provider", digest)
		}
		return failure
	})
	return result, err
}

func TestEffectBindingDistinguishesEarlyDeletionAndRetainedState(t *testing.T) {
	s, _ := database(t, false)
	ctx := context.Background()
	digest := strings.Repeat("a", 64)
	if got, err := s.LoadEffectBinding(ctx, "Record", "early", "provider"); err != nil || got.Present {
		t.Fatal("absent binding differs", got, err)
	}
	for range 2 {
		got, err := effectWrite(ctx, s, "early", "", true)
		if err != nil || !got.Present || !got.Closed || got.Digest != "" {
			t.Fatal("early deletion did not close registration", got, err)
		}
	}
	if _, err := effectWrite(ctx, s, "early", digest, false); !errors.Is(err, contract.ErrEffectBindingConflict) {
		t.Fatal("late effect registered after early deletion", err)
	}
	for range 2 {
		got, err := effectWrite(ctx, s, "started", digest, false)
		if err != nil || !got.Present || got.Closed || got.Digest != digest {
			t.Fatal("same-state registration failed", got, err)
		}
	}
	if _, err := effectWrite(ctx, s, "started", strings.Repeat("b", 64), false); !errors.Is(err, contract.ErrEffectBindingConflict) {
		t.Fatal("binding replacement was accepted", err)
	}
	for range 2 {
		got, err := effectWrite(ctx, s, "started", "", true)
		if err != nil || !got.Closed || got.Digest != digest {
			t.Fatal("closure lost required state", got, err)
		}
	}
	next, err := NewStore(s.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"early", "started"} {
		got, err := next.LoadEffectBinding(ctx, "Record", id, "provider")
		if err != nil || !got.Present || !got.Closed || (id == "early") != (got.Digest == "") {
			t.Fatal("store replacement lost registration history", got, err)
		}
		if _, err := effectWrite(ctx, next, id, digest, false); !errors.Is(err, contract.ErrEffectBindingConflict) {
			t.Fatal("closed binding reopened after restart", err)
		}
	}
	for _, key := range []struct{ id, scope string }{{"Started", "provider"}, {"started", "Provider"}} {
		if got, err := next.LoadEffectBinding(ctx, "Record", key.id, key.scope); err != nil || got.Present {
			t.Fatal("binding keys are not exact", got, err)
		}
	}
}

func TestEffectBindingFailureRollsBackAndRequiresActiveTransaction(t *testing.T) {
	s, db := database(t, false)
	ctx := context.Background()
	digest := strings.Repeat("c", 64)
	if _, err := s.BindEffect(ctx, "Record", "id", "provider", digest); !errors.Is(err, ErrTransactionRequired) {
		t.Fatal(err)
	}
	if _, err := s.CloseEffectBinding(ctx, "Record", "id", "provider"); !errors.Is(err, ErrTransactionRequired) {
		t.Fatal(err)
	}
	var retained contract.EffectBindingStore
	err := s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		retained = tx.(contract.EffectBindingStore)
		if _, err := retained.BindEffect(ctx, "Record", "id", "provider", digest); err != nil {
			return err
		}
		_, _ = retained.BindEffect(ctx, "Record", "id", "provider", strings.Repeat("d", 64))
		return nil
	})
	if !errors.Is(err, contract.ErrEffectBindingConflict) || count(t, db, "stego_effect_bindings") != 0 {
		t.Fatal("ignored failure committed state", err)
	}
	if _, err := retained.BindEffect(ctx, "Record", "id", "provider", digest); !errors.Is(err, ErrTransactionClosed) {
		t.Fatal(err)
	}
	if _, err := retained.CloseEffectBinding(ctx, "Record", "id", "provider"); !errors.Is(err, ErrTransactionClosed) {
		t.Fatal(err)
	}
	if _, err := retained.LoadEffectBinding(ctx, "Record", "id", "provider"); !errors.Is(err, ErrTransactionClosed) {
		t.Fatal(err)
	}
	for _, bad := range []struct{ entity, id, scope, digest string }{{"Unknown", "id", "provider", digest}, {"Record", "", "provider", digest}, {"Record", "id", strings.Repeat("s", 129), digest}, {"Record", "id", "provider", ""}, {"Record", "id", "provider", strings.Repeat("A", 64)}, {"Record", "id\x00", "provider", digest}} {
		err := s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
			_, err := tx.(contract.EffectBindingStore).BindEffect(ctx, bad.entity, bad.id, bad.scope, bad.digest)
			return err
		})
		if !errors.Is(err, contract.ErrEffectBinding) {
			t.Fatal("invalid binding accepted", err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := effectWrite(cancelled, s, "cancelled", digest, false); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled registration accepted", err)
	}
	if count(t, db, "stego_effect_bindings") != 0 {
		t.Fatal("invalid registration wrote state")
	}
}

func TestEffectBindingSerializesRegistrationAndClosure(t *testing.T) {
	s, _ := database(t, false)
	ctx := context.Background()
	digest := strings.Repeat("e", 64)
	for attempt := range 8 {
		id := strings.Repeat("x", attempt+1)
		start := make(chan struct{})
		bound := make(chan error, 1)
		go func() { <-start; _, err := effectWrite(ctx, s, id, digest, false); bound <- err }()
		close(start)
		closed, err := effectWrite(ctx, s, id, "", true)
		bindErr := <-bound
		// A serializable transaction can reject one concurrent operation.
		// It committed no effect. Repeat only after both transactions finish.
		if errors.Is(err, contract.ErrSerialization) {
			closed, err = effectWrite(ctx, s, id, "", true)
		}
		if errors.Is(bindErr, contract.ErrSerialization) {
			_, bindErr = effectWrite(ctx, s, id, digest, false)
		}
		if err != nil {
			t.Fatal(err)
		}
		if !closed.Present || !closed.Closed {
			t.Fatal("closure missing", closed)
		}
		if closed.Digest == "" {
			if !errors.Is(bindErr, contract.ErrEffectBindingConflict) {
				t.Fatal("early closure accepted registration", bindErr)
			}
		} else if closed.Digest != digest || bindErr != nil {
			t.Fatal("closure lost accepted registration", closed, bindErr)
		}
		if _, err := effectWrite(ctx, s, id, digest, false); !errors.Is(err, contract.ErrEffectBindingConflict) {
			t.Fatal("late registration succeeded", err)
		}
	}
}

func TestEffectBindingSchemaMustPreserveBoundsAndIdentity(t *testing.T) {
	for _, change := range []string{
		"ALTER TABLE stego_effect_bindings DROP CONSTRAINT stego_effect_bindings_pkey",
		"ALTER TABLE stego_effect_bindings DROP CONSTRAINT stego_effect_bindings_check",
		"ALTER TABLE stego_effect_bindings ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE stego_effect_bindings DISABLE TRIGGER stego_effect_binding_guard",
	} {
		t.Run(change, func(t *testing.T) {
			s, db := database(t, false)
			if _, err := db.Exec(change); err != nil {
				t.Fatal(err)
			}
			if _, err := NewStore(s.db); err == nil {
				t.Fatal("invalid effect binding schema accepted")
			}
		})
	}
}

func TestEffectBindingHistoryCannotBeChangedOrRemoved(t *testing.T) {
	s, db := database(t, false)
	ctx := context.Background()
	digest := strings.Repeat("f", 64)
	if _, err := effectWrite(ctx, s, "history", digest, false); err != nil {
		t.Fatal(err)
	}
	if _, err := effectWrite(ctx, s, "history", "", true); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"DELETE FROM stego_effect_bindings",
		"UPDATE stego_effect_bindings SET closed=false",
		"UPDATE stego_effect_bindings SET digest=repeat('a',64)",
		"UPDATE stego_effect_bindings SET resource_id='other'",
		"UPDATE stego_effect_bindings SET scope='other'",
		"UPDATE stego_effect_bindings SET entity='other'",
	} {
		if _, err := db.Exec(query); err == nil {
			t.Fatal("effect binding history changed", query)
		}
	}
	got, err := s.LoadEffectBinding(ctx, "Record", "history", "provider")
	if err != nil || !got.Present || !got.Closed || got.Digest != digest {
		t.Fatal("effect binding history was lost", got, err)
	}
}

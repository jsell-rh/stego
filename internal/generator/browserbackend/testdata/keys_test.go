package browser

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func keyRing(t *testing.T, keys ...[]byte) []byte {
	t.Helper()
	encoded := make([]string, len(keys))
	for i, key := range keys {
		encoded[i] = base64.StdEncoding.EncodeToString(key)
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "keys": encoded})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSessionKeyFile(t *testing.T) {
	a, b, c := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 32)
	encoded := base64.StdEncoding.EncodeToString(a)
	for _, raw := range [][]byte{[]byte(encoded + "\n"), keyRing(t, a), keyRing(t, a, b), keyRing(t, a, b, c)} {
		keys, err := sessionKeys(raw)
		require(t, err == nil && len(keys) > 0 && bytes.Equal(keys[0], a), "valid key file rejected")
		if len(keys) > 1 {
			require(t, bytes.Equal(keys[1], b), "read key order changed")
		}
	}
	cases := map[string]string{
		"empty": "", "space": " \n", "oversize": strings.Repeat(" ", 1025) + encoded,
		"short": base64.StdEncoding.EncodeToString(a[:31]), "long": base64.StdEncoding.EncodeToString(append(a, 1)),
		"padding": strings.TrimSuffix(encoded, "="), "noncanonical": encoded[:42] + "F=", "line-break": encoded[:8] + "\n" + encoded[8:],
		"json-string": `"` + encoded + `"`, "array": `["` + encoded + `"]`, "null": `null`,
		"missing-version": `{"keys":["` + encoded + `"]}`, "unknown-version": `{"version":2,"keys":["` + encoded + `"]}`,
		"float-version": `{"version":1.0,"keys":["` + encoded + `"]}`, "case-version": `{"Version":1,"keys":["` + encoded + `"]}`,
		"unknown-field": `{"version":1,"keys":["` + encoded + `"],"extra":0}`, "duplicate-field": `{"version":1,"version":1,"keys":["` + encoded + `"]}`,
		"duplicate-escaped-field": `{"version":1,"keys":[],"\u006beys":["` + encoded + `"]}`,
		"empty-ring":              `{"version":1,"keys":[]}`, "null-ring": `{"version":1,"keys":null}`, "string-ring": `{"version":1,"keys":"` + encoded + `"}`,
		"null-key": `{"version":1,"keys":[null]}`, "duplicate-key": string(keyRing(t, a, a)),
		"too-many": string(keyRing(t, a, b, c, bytes.Repeat([]byte{4}, 32))), "invalid-second": string(keyRing(t, a, []byte{2})),
		"trailing": string(keyRing(t, a)) + ` {}`, "invalid-unicode": `{"version":1,"keys":["\ud800"]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			keys, err := sessionKeys([]byte(raw))
			require(t, errors.Is(err, errSessionKeys) && keys == nil, "invalid key file accepted or key material returned")
		})
	}
}

func TestSessionKeyRollout(t *testing.T) {
	db := database(t)
	migrate(t, db)
	ctx := context.Background()
	old, next, third := bytes.Repeat([]byte{7}, 32), bytes.Repeat([]byte{8}, 32), bytes.Repeat([]byte{9}, 32)
	store := func(keys ...[]byte) *sessionStore {
		t.Helper()
		s, err := newStore(ctx, db, keys...)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	legacy, prepared, switched, retired := store(old), store(old, next), store(next, old, third), store(next)
	id, _ := randomValue()
	value := session{Kind: "active", Expires: time.Now().Add(time.Hour).Unix(), Access: "access", Refresh: "refresh", CSRF: "csrf"}
	// Use the old wire format without the store encoder to check compatibility.
	hash, _ := sessionHash(id)
	plain, _ := json.Marshal(value)
	block, _ := aes.NewCipher(old)
	aead, _ := cipher.NewGCM(block)
	nonce := bytes.Repeat([]byte{5}, aead.NonceSize())
	payload := aead.Seal(nonce, nonce, plain, hash)
	if _, err := db.ExecContext(ctx, "INSERT INTO stego_browser_sessions(id_hash,payload,state,expires_at) VALUES($1,$2,'active',$3)", hash, payload, time.Unix(value.Expires, 0)); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*sessionStore{legacy, prepared, switched} {
		got, state, _, err := s.read(ctx, id)
		require(t, err == nil && state == "active" && got.Refresh == value.Refresh && got.Expires == value.Expires, "rollout lost the old session")
	}
	_, _, _, err := retired.read(ctx, id)
	require(t, errors.Is(err, errSession), "removed key still decrypts old records")
	// Both writers must work during the switch. Refresh must preserve expiry.
	for _, writer := range []*sessionStore{prepared, switched, prepared, switched} {
		got, err := writer.claimRefresh(ctx, id)
		require(t, err == nil, "mixed writer cannot claim refresh")
		require(t, writer.finishRefresh(ctx, id, got) == nil, "mixed writer cannot finish refresh")
		for _, reader := range []*sessionStore{prepared, switched} {
			got, state, _, err := reader.read(ctx, id)
			require(t, err == nil && state == "active" && got.Expires == value.Expires && got.CSRF == value.CSRF, "mixed reader lost session fields")
		}
	}
	got, _, _, err := retired.read(ctx, id)
	require(t, err == nil && got.Refresh == value.Refresh, "new write key was not used")
	_, _, _, err = legacy.read(ctx, id)
	require(t, errors.Is(err, errSession), "old key decrypts a new record")
	// A retained third key must enforce the same record binding and size limits.
	payload, err = store(third).seal(id, value)
	require(t, err == nil, "third key seal failed")
	_, err = switched.open(id, payload)
	require(t, err == nil, "third read key was not used")
	other, _ := randomValue()
	_, err = switched.open(other, payload)
	require(t, errors.Is(err, errSession), "retained key lost cookie binding")
	changed := bytes.Clone(payload)
	changed[len(changed)-1] ^= 1
	for _, bad := range [][]byte{nil, payload[:8], changed, make([]byte, 65537)} {
		_, err = switched.open(id, bad)
		require(t, errors.Is(err, errSession), "invalid retained ciphertext accepted")
	}
	value.Expires = time.Now().Add(-time.Second).Unix()
	payload, err = store(third).seal(id, value)
	require(t, err == nil, "expired fixture seal failed")
	_, err = switched.open(id, payload)
	require(t, errors.Is(err, errSession), "retained key restored expired session")
	// A refresh that spans a key switch cannot restore a signed-out session.
	got, err = prepared.claimRefresh(ctx, id)
	require(t, err == nil, "refresh claim failed")
	require(t, switched.remove(ctx, id) == nil, "logout during switch failed")
	require(t, prepared.finishRefresh(ctx, id, got) != nil, "refresh restored signed-out session")
	for _, keys := range [][][]byte{nil, {old, old}, {old[:31]}, {old, next, third, bytes.Repeat([]byte{10}, 32)}} {
		_, err := newStore(ctx, db, keys...)
		require(t, errors.Is(err, errSessionKeys), "invalid store key ring accepted")
	}
}

func TestBackendSessionKeyRotation(t *testing.T) {
	f := setup(t)
	active, csrf := login(t, f)
	raw, err := os.ReadFile(f.options.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := sessionKeys(raw)
	if err != nil {
		t.Fatal(err)
	}
	next := bytes.Repeat([]byte{12}, 32)
	start := func(keys ...[]byte) *Backend {
		t.Helper()
		o := f.options
		o.KeyFile = privateFile(t, "session-keys", keyRing(t, keys...))
		b, err := newBackend(context.Background(), f.db, o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(b.Close)
		return b
	}
	prepared, switched := start(keys[0], next), start(next, keys[0])
	for _, b := range []*Backend{prepared, switched} {
		w := send(b, "GET", "/auth/session", "", []*http.Cookie{active}, nil)
		require(t, w.Code == 200 && strings.Contains(w.Body.String(), csrf), "backend key switch lost the browser session")
	}
	// The key file is checked before database or provider access.
	o := f.options
	o.KeyFile = privateFile(t, "invalid-keys", keyRing(t, next, next))
	b, err := newBackend(context.Background(), nil, o)
	require(t, b == nil && errors.Is(err, errSessionKeys), "invalid key ring did not fail at startup")
	w := send(switched, "POST", "/auth/logout", "", []*http.Cookie{active}, mutationHeaders(csrf))
	require(t, w.Code == 204, "logout after key switch failed")
	w = send(prepared, "GET", apiPrefix+"/records", "", []*http.Cookie{active}, nil)
	require(t, w.Code == 401, "old writer restored signed-out session")
}

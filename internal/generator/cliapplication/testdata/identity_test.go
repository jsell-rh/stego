package command

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdentityDefinition(t *testing.T) {
	app := Application{ConfigEnv: "TEST_CLI_CONFIG", ConfigName: "sample", IdentityPath: "/identity"}
	if err := validate(app); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"https://other.test/identity", "//other.test/identity", "/identity?token=x", "/identity/{id}", "/../identity"} {
		invalid := app
		invalid.IdentityPath = path
		if err := validate(invalid); err == nil {
			t.Fatal("invalid identity destination accepted", path)
		}
	}
	app.Commands = []Command{{Name: []string{"whoami"}, Method: "GET", Path: "/identity", Success: []int{200}}}
	if err := validate(app); err == nil {
		t.Fatal("identity command collision accepted")
	}
}
func TestVerifiedIdentityAndProtectedTokenOutput(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("TEST_CLI_CONFIG", filepath.Join(directory, "config.json"))
	payload := `{"sub":"local-claim","private":"sensitive-claim","number":9007199254740993}`
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
	tokenFile := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	var calls, mode atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/identity" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("incorrect identity request")
		}
		w.Header().Set("Content-Type", "application/json")
		if mode.Load() == 1 {
			w.WriteHeader(401)
			w.Write([]byte(token))
			return
		}
		if mode.Load() == 2 {
			w.Write([]byte(`{"username":"unknown"}`))
			return
		}
		if mode.Load() == 3 {
			w.Write([]byte(`{"username":"a","username":"b"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"username": "verified-user", "email": "user@example.test", "subject": "verified-subject", "issuer": "https://issuer.test", "expires_at": time.Now().Add(time.Hour).UTC(), "private": "must-not-copy"})
	}))
	defer server.Close()
	ca := filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	app := Application{ConfigEnv: "TEST_CLI_CONFIG", ConfigName: "sample", IdentityPath: "/identity"}
	run := func(args ...string) (string, error) {
		var output bytes.Buffer
		err := Run(context.Background(), app, args, &output)
		return output.String(), err
	}
	if _, err := run("login", "--url", server.URL, "--ca-file", ca, "--token-file", tokenFile); err != nil {
		t.Fatal(err)
	}
	value, err := run("whoami")
	if err != nil {
		t.Fatal(err)
	}
	var report IdentityReport
	if json.Unmarshal([]byte(value), &report) != nil || report.Username != "verified-user" || report.Subject != "verified-subject" || report.APIURL != server.URL || report.ExpiresAt.IsZero() {
		t.Fatal("identity was not read from the API")
	}
	for _, private := range []string{token, "local-claim", "sensitive-claim", "must-not-copy"} {
		if strings.Contains(value, private) {
			t.Fatal("default output copied private or local claims")
		}
	}
	for _, args := range [][]string{{"--show-token"}, {"-t"}, {"--show-token-decoded"}, {"--show-token", "--show-token-decoded", "--output-file", "-"}, {"--show-token", "-t", "--output-file", "-"}, {"--unknown"}, {"extra"}} {
		before := calls.Load()
		value, err := run(append([]string{"whoami"}, args...)...)
		if err == nil || value != "" || calls.Load() != before {
			t.Fatal("invalid token output contacted API", err)
		}
	}
	for _, flag := range []string{"--show-token", "-t", "--show-token-decoded"} {
		output := filepath.Join(directory, strings.TrimLeft(flag, "-")+".txt")
		if value, err := run("whoami", flag, "--output-file", output); err != nil || value != "" {
			t.Fatal("protected output failed", err)
		}
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(output)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("token output is not private")
		}
		if flag == "--show-token-decoded" {
			if !bytes.Contains(data, []byte("9007199254740993")) || !bytes.Contains(data, []byte("sensitive-claim")) {
				t.Fatal("decoded claims changed")
			}
		} else if string(data) != token+"\n" {
			t.Fatal("raw token changed")
		}
		before := calls.Load()
		if _, err := run("whoami", flag, "--output-file", output); err == nil || calls.Load() != before {
			t.Fatal("existing token output contacted API")
		}
	}
	if value, err := run("whoami", "-t", "--output-file", "-"); err != nil || value != token+"\n" {
		t.Fatal("explicit token stdout failed", err)
	}
	for _, failure := range []int32{1, 2, 3} {
		mode.Store(failure)
		for _, flag := range []string{"--show-token", "--show-token-decoded"} {
			output := filepath.Join(directory, "failed")
			value, err := run("whoami", flag, "--output-file", output)
			if err == nil || value != "" || strings.Contains(err.Error(), token) {
				t.Fatal("failed identity disclosed token", err)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("failed identity left an output file", err)
			}
		}
	}
}

package authntest_test

import (
	"context"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
)

func TestServer(t *testing.T) {
	t.Run("ST-SUP-RP13-LC16-T01 authntest serves keys locally and is used only by tests", func(t *testing.T) {
		var address string
		t.Run("serve", func(t *testing.T) {
			server := authntest.NewServer(t)
			address = server.Addr()
			host, _, err := net.SplitHostPort(address)
			if err != nil || host != "127.0.0.1" {
				t.Errorf("the server listens on %q, want 127.0.0.1", address)
			}
			key := authntest.NewKey(t, "k1")
			server.Publish(key)
			keys, err := authn.NewKeySet(context.Background(), authn.KeySource{URL: server.URL(), CAFile: server.CAFile(t)})
			if err != nil {
				t.Fatalf("load the document through the remote source: %v", err)
			}
			defer keys.Stop()
			const issuer = "https://issuer.authn.test/realms/r1"
			v, err := authn.New(authn.Settings{Issuer: issuer, Audience: "orders-api", ProblemCode: "SVC", Keys: keys})
			if err != nil {
				t.Fatal(err)
			}
			token := authntest.Mint(t, authntest.Header("k1"), authntest.Claims(issuer, "orders-api", "user-1", time.Now()), key)
			id, err := v.Verify(context.Background(), token)
			if err != nil || id.UserID() != "user-1" {
				t.Fatalf("Verify gave %+v, %v", id, err)
			}
			if server.Requests() != 1 {
				t.Errorf("the server got %d requests, want 1", server.Requests())
			}
		})
		if address == "" {
			t.Fatal("the server did not start")
		}
		if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
			_ = conn.Close()
			t.Errorf("the server at %s still accepts connections after its test ended", address)
		}

		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t.Fatal(err)
		}
		const path = "github.com/jsell-rh/stego/registry/runtime/authn/authntest"
		scanned := 0
		fset := token.NewFileSet()
		err = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			scanned++
			for _, spec := range file.Imports {
				if value, _ := strconv.Unquote(spec.Path.Value); value == path {
					t.Errorf("the file %s is not a test file and imports authntest", name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if scanned < 10 {
			t.Fatalf("scanned %d files under %s, want the whole module", scanned, root)
		}
	})
}

package httpapplication_test

import (
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/httpapplication"
)

func TestApplicationRejectsInvalidFactory(t *testing.T) {
	for _, factory := range []any{nil, 42, "../escape", "out/domain", "/tmp/domain", ""} {
		_, _, err := new(httpapplication.Generator).Generate(gen.Context{OutputNamespace: "application", OutDirName: "out", ModuleName: "example.com/service", AuthPackage: "example.com/service/out/auth", StorageContract: "example.com/service/out/contracts/storage", PeerNamespaces: map[string]string{"jwt-auth": "auth"}, ComponentConfig: map[string]any{"factory_package": factory}})
		if err == nil {
			t.Fatalf("invalid factory was accepted: %v", factory)
		}
	}
}

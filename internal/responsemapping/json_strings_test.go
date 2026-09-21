package responsemapping

import (
	"crypto/sha256"
	"fmt"
	"go/parser"
	"go/token"
	"testing"
)

func TestJSONStringsSource(t *testing.T) {
	for _, name := range []string{"mapping", "responses"} {
		source, err := JSONStringsSource(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), "json_strings.go", source, 0)
		if err != nil || f.Name.Name != name {
			t.Fatal("invalid converter package", err)
		}
		if name == "mapping" && fmt.Sprintf("%x", sha256.Sum256(source)) != "690ad359b58b3c717028bae5e2a9b8accfa06f9fe7743d21ee116fb0494b9e42" {
			t.Fatal("shared decoder changed the accepted gRPC output")
		}
	}
	for _, name := range []string{"", "_", "package", "a.b", "x\nimport \"os\""} {
		if source, err := JSONStringsSource(name); err == nil || source != nil {
			t.Fatal("invalid converter package returned code")
		}
	}
}

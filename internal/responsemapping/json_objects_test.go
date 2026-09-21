package responsemapping

import (
	"go/parser"
	"go/token"
	"testing"
)

func TestJSONObjectsSource(t *testing.T) {
	for _, name := range []string{"mapping", "responses"} {
		source, err := JSONObjectsSource(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), "json_objects.go", source, 0)
		if err != nil || file.Name.Name != name {
			t.Fatal("invalid object decoder package", err)
		}
	}
	for _, name := range []string{"", "_", "package", "a.b", "x\nimport \"os\""} {
		if source, err := JSONObjectsSource(name); err == nil || source != nil {
			t.Fatal("invalid package returned source")
		}
	}
}

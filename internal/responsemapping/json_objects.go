package responsemapping

import (
	_ "embed"
	"fmt"
	"go/token"
	"strings"
)

//go:embed json_objects.go.tmpl
var jsonObjectsSource string

// JSONObjectsSource uses the Unicode check from JSONStringsSource.
// The caller emits both sources once in the same package.
func JSONObjectsSource(packageName string) ([]byte, error) {
	if !token.IsIdentifier(packageName) || packageName == "_" {
		return nil, fmt.Errorf("invalid response mapping package")
	}
	return []byte(strings.Replace(jsonObjectsSource, "package mapping\n", "package "+packageName+"\n", 1)), nil
}

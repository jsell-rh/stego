// Package responsemapping supplies checked conversion code for generated adapters.
package responsemapping

import (
	_ "embed"
	"fmt"
	"go/token"
	"strings"
)

//go:embed json_strings.go.tmpl
var jsonStringsSource string

// JSONStringsSource returns the common bounded decoder in the selected package.
func JSONStringsSource(packageName string) ([]byte, error) {
	if !token.IsIdentifier(packageName) || packageName == "_" {
		return nil, fmt.Errorf("invalid response mapping package")
	}
	return []byte(strings.Replace(jsonStringsSource, "package mapping\n", "package "+packageName+"\n", 1)), nil
}

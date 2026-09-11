package compiler

import (
	"fmt"
	"go/ast"
	"go/token"
	"path"
	"slices"

	"github.com/jsell-rh/stego/internal/gen"
)

func validateDatabaseOpener(wirings []ComponentWiring) error {
	seen := false
	for _, component := range wirings {
		w := component.Wiring
		if w == nil || w.DatabaseOpener == nil {
			continue
		}
		opener := w.DatabaseOpener
		if seen {
			return fmt.Errorf("multiple components declare a database opener")
		}
		seen = true
		if !w.NeedsDB || (w.DBBackend != "" && w.DBBackend != "gorm") {
			return fmt.Errorf("component %q declares a database opener without a supported database resource", component.Name)
		}
		if gen.ValidateGoPackageNamespace(opener.Namespace) != nil || !slices.Contains(w.Imports, opener.Namespace) || !token.IsIdentifier(opener.Function) || !ast.IsExported(opener.Function) {
			return fmt.Errorf("component %q has an invalid database opener", component.Name)
		}
	}
	return nil
}

func databaseOpenExpression(input AssemblerInput, imports importResult, consumed map[int]bool) string {
	for i, component := range input.Wirings {
		if !consumed[i] || component.Wiring == nil || component.Wiring.DatabaseOpener == nil {
			continue
		}
		opener := component.Wiring.DatabaseOpener
		name := path.Base(opener.Namespace)
		if renamed := imports.Renames[i][name]; renamed != "" {
			name = renamed
		}
		return name + "." + opener.Function + "(dsn)"
	}
	return ""
}

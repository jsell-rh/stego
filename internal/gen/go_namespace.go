package gen

import (
	"fmt"
	"go/token"
	"path"

	"golang.org/x/mod/module"
)

// ValidateGoImportNamespace checks a canonical directory used in a Go import.
// A directory that contains packages need not itself name a Go package.
func ValidateGoImportNamespace(namespace string) error {
	if err := ValidatePath(namespace); err != nil {
		return err
	}
	if err := module.CheckImportPath(namespace); err != nil {
		return fmt.Errorf("invalid Go import namespace %q: %w", namespace, err)
	}
	return nil
}

// ValidateGoPackageNamespace also checks the library package name derived from
// the final path segment. A main package cannot be imported as a library.
func ValidateGoPackageNamespace(namespace string) error {
	if err := ValidateGoImportNamespace(namespace); err != nil {
		return err
	}
	return ValidateGoLibraryName(path.Base(namespace))
}

// ValidateGoLibraryName checks a library name after a generator derives it.
// Generators such as protobuf can derive a name that differs from its directory.
func ValidateGoLibraryName(name string) error {
	if !token.IsIdentifier(name) || name == "_" || name == "main" {
		return fmt.Errorf("Go library package name %q is not importable", name)
	}
	return nil
}

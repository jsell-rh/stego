package compiler

import (
	"fmt"
	"go/version"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// validateBuildTarget is shared by project settings, validation, and assembly.
func validateBuildTarget(moduleName, goVersion string) error {
	if moduleName == "" {
		return fmt.Errorf("ModuleName must not be empty")
	}
	if goVersion == "" {
		return fmt.Errorf("GoVersion must not be empty")
	}
	if err := module.CheckImportPath(moduleName); err != nil {
		return fmt.Errorf("invalid module name: %w", err)
	}
	if !version.IsValid("go" + goVersion) {
		return fmt.Errorf("invalid Go version %q", goVersion)
	}
	if err := new(modfile.File).AddGoStmt(goVersion); err != nil {
		return fmt.Errorf("invalid Go version: %w", err)
	}
	return nil
}

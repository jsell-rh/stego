package main

import (
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

func fillPackageName(name string) (string, error) {
	if err := gen.ValidatePath(name); err != nil || strings.Contains(name, "/") {
		return "", fmt.Errorf("fill name must be one safe path segment: %q", name)
	}
	packageName := strings.ReplaceAll(name, "-", "_")
	if !token.IsIdentifier(packageName) || packageName == "_" || packageName == "main" {
		return "", fmt.Errorf("fill name %q does not form a Go package name", name)
	}
	return packageName, nil
}

// writeFillScaffold creates each path exclusively. Existing application files
// cannot be overwritten. All source validation must finish before this call.
func writeFillScaffold(projectDir, name string, declaration, source []byte) error {
	if _, err := fillPackageName(name); err != nil {
		return err
	}
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Mkdir("fills", 0755); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := root.Lstat("fills")
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("fills must be a directory without symbolic links")
	}
	fills, err := root.OpenRoot("fills")
	if err != nil {
		return err
	}
	defer fills.Close()
	if err := fills.Mkdir(name, 0755); err != nil {
		return fmt.Errorf("creating fill directory (it must not already exist): %w", err)
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"fill.yaml", declaration}, {"fill.go", source}} {
		f, err := fills.OpenFile(filepath.Join(name, file.name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		_, writeErr := f.Write(file.data)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

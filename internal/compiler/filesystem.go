package compiler

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

func outputRelative(projectDir, outDir string) (string, error) {
	relative, err := filepath.Rel(projectDir, outDir)
	if err != nil || gen.ValidatePath(filepath.ToSlash(relative)) != nil {
		return "", fmt.Errorf("OutDir must be a subdirectory of ProjectDir")
	}
	first := strings.Split(filepath.ToSlash(relative), "/")[0]
	if first == "fills" || first == ".stego" || first == ".git" {
		return "", fmt.Errorf("OutDir must not use reserved directory %q", first)
	}
	return relative, nil
}

func projectFilePath(outDir, name string) string {
	if isProjectRootFile(name) {
		return name
	}
	return filepath.Join(outDir, name)
}

func checkFileTarget(root *os.Root, name string) error {
	if err := checkFilePath(root, name); err != nil {
		return err
	}
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("output target %s must be a regular file", name)
	}
	return nil
}

// checkFilePath rejects links and special files before apply starts. Root
// operations also enforce containment during each filesystem operation.
func checkFilePath(root *os.Root, name string) error {
	if err := gen.ValidatePath(filepath.ToSlash(name)); err != nil {
		return err
	}
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i := range parts {
		path := filepath.Join(parts[:i+1]...)
		info, err := root.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("checking %s: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symbolic link at %s", path)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("path parent %s must be a directory", path)
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("path %s must be a regular file or directory", path)
		}
	}
	return nil
}

// writeRootFile replaces a complete file through a rename within the root.
// Readers cannot observe a truncated file. A failed write retains the old file.
// This does not make a multi-file apply atomic.
func writeRootFile(root *os.Root, name string, data []byte) (err error) {
	if err := checkFilePath(root, name); err != nil {
		return err
	}
	directory := filepath.Dir(name)
	if err := root.MkdirAll(directory, 0755); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("creating temporary file name: %w", err)
	}
	temporary := filepath.Join(directory, ".stego-write-"+hex.EncodeToString(nonce[:]))
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := root.Remove(temporary); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("removing temporary file: %w", removeErr))
		}
	}()
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err := root.Rename(temporary, name); err != nil {
		return err
	}
	return nil
}

package gen

import (
	"errors"
	"testing"
)

func TestValidateNamespace_AllFilesInside(t *testing.T) {
	files := []File{
		{Path: "internal/api/handler.go", Content: []byte("package api")},
		{Path: "internal/api/routes.go", Content: []byte("package api")},
		{Path: "internal/api/sub/deep.go", Content: []byte("package sub")},
	}
	if err := ValidateNamespace("internal/api", files); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateNamespace_FileOutside(t *testing.T) {
	files := []File{
		{Path: "internal/api/handler.go", Content: []byte("package api")},
		{Path: "internal/storage/models.go", Content: []byte("package storage")},
		{Path: "cmd/main.go", Content: []byte("package main")},
	}
	err := ValidateNamespace("internal/api", files)
	if err == nil {
		t.Fatal("expected error for files outside namespace")
	}

	var nsErr *NamespaceError
	if !errors.As(err, &nsErr) {
		t.Fatalf("expected *NamespaceError, got %T", err)
	}
	if nsErr.Namespace != "internal/api" {
		t.Errorf("expected namespace %q, got %q", "internal/api", nsErr.Namespace)
	}
	if len(nsErr.Violations) != 2 {
		t.Fatalf("expected 2 violations, got %d: %v", len(nsErr.Violations), nsErr.Violations)
	}
}

func TestValidateNamespace_EmptyNamespace(t *testing.T) {
	files := []File{{Path: "foo.go", Content: []byte("package foo")}}
	err := ValidateNamespace("", files)
	if err == nil {
		t.Fatal("expected error for empty namespace")
	}
}

func TestValidateNamespace_DotNamespace(t *testing.T) {
	files := []File{{Path: "foo.go", Content: []byte("package foo")}}
	err := ValidateNamespace(".", files)
	if err == nil {
		t.Fatal("expected error for dot namespace")
	}
}

func TestValidateNamespace_TraversalRejected(t *testing.T) {
	files := []File{
		{Path: "internal/api/../storage/models.go", Content: []byte("package storage")},
	}
	err := ValidateNamespace("internal/api", files)
	if err == nil {
		t.Fatal("expected error for path traversal outside namespace")
	}
	var nsErr *NamespaceError
	if !errors.As(err, &nsErr) {
		t.Fatalf("expected *NamespaceError, got %T", err)
	}
}

func TestValidateNamespace_EmptyFileList(t *testing.T) {
	if err := ValidateNamespace("internal/api", nil); err != nil {
		t.Fatalf("expected no error for empty file list, got: %v", err)
	}
}

func TestValidateNamespace_PrefixOverlap(t *testing.T) {
	// "internal/api-v2/handler.go" should NOT match namespace "internal/api"
	files := []File{
		{Path: "internal/api-v2/handler.go", Content: []byte("package apiv2")},
	}
	err := ValidateNamespace("internal/api", files)
	if err == nil {
		t.Fatal("expected error: internal/api-v2 is not under internal/api/")
	}
}

func TestFileWithHeader(t *testing.T) {
	f := File{
		Path:    "internal/api/handler.go",
		Content: []byte("package api"),
	}
	got := string(f.WithHeader())
	want := Header + "\n\npackage api"
	if got != want {
		t.Errorf("WithHeader() =\n%s\nwant:\n%s", got, want)
	}
}

func TestNamespaceErrorMessage(t *testing.T) {
	e := &NamespaceError{
		Namespace:  "internal/api",
		Violations: []string{"cmd/main.go", "internal/storage/models.go"},
	}
	msg := e.Error()
	if msg == "" {
		t.Fatal("expected non-empty error message")
	}
	if want := `files outside namespace "internal/api": cmd/main.go, internal/storage/models.go`; msg != want {
		t.Errorf("got: %s\nwant: %s", msg, want)
	}
}

package main

import (
	"os"
	"testing"
)

func TestRecoverWithoutTransactionDoesNotNeedRegistry(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	t.Setenv("STEGO_REGISTRY", "/missing-registry")
	if err := runRecover(nil); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(project)
	if err != nil || len(entries) != 0 {
		t.Fatalf("recovery without a transaction changed the project: %v", err)
	}
}

func TestRecoverRejectsArguments(t *testing.T) {
	if err := runRecover([]string{"unexpected"}); err == nil {
		t.Fatal("unexpected argument was ignored")
	}
}

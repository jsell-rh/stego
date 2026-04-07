package healthcheck_test

import (
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/healthcheck"
)

func TestGeneratorImplementsInterface(t *testing.T) {
	var _ gen.Generator = (*healthcheck.Generator)(nil)
}

func TestGenerateReturnsEmptyFileList(t *testing.T) {
	g := &healthcheck.Generator{}
	files, wiring, err := g.Generate(gen.Context{})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d", len(files))
	}
	if wiring != nil {
		t.Errorf("expected nil wiring, got %+v", wiring)
	}
}

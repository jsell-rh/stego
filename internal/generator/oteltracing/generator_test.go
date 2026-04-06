package oteltracing_test

import (
	"testing"

	"github.com/stego-project/stego/internal/gen"
	"github.com/stego-project/stego/internal/generator/oteltracing"
)

func TestGeneratorImplementsInterface(t *testing.T) {
	var _ gen.Generator = (*oteltracing.Generator)(nil)
}

func TestGenerateReturnsEmptyFileList(t *testing.T) {
	g := &oteltracing.Generator{}
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

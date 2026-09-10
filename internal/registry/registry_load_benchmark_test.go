package registry_test

import (
	"testing"

	"github.com/jsell-rh/stego/internal/registry"
)

func BenchmarkRegistryLoad(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := registry.Load("../../registry"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRegistryVerify(b *testing.B) {
	source, err := registry.Load("../../registry")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := source.Verify(); err != nil {
			b.Fatal(err)
		}
	}
}

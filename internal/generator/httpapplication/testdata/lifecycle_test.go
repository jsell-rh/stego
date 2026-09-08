package sample_test

import (
	"context"
	"testing"
	"time"

	"example.com/http-test/out/application"
	"example.com/http-test/out/auth"
	"example.com/http-test/sample"
)

func TestManagedApplicationLifecycle(t *testing.T) {
	sample.Managed.Store(true)
	defer sample.Managed.Store(false)
	runtime, err := application.NewHandler(nil, &auth.Verifier{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("managed task did not stop")
	}
	runtime.Close()
	runtime.Close()
	if sample.Runs.Load() != 1 || sample.Closes.Load() != 1 {
		t.Fatalf("managed lifecycle: runs=%d closes=%d", sample.Runs.Load(), sample.Closes.Load())
	}
	sample.Managed.Store(false)
	plain, err := application.NewHandler(nil, &auth.Verifier{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.Run(ctx); err != nil {
		t.Fatal(err)
	}
	plain.Close()
	if sample.Closes.Load() != 1 {
		t.Fatal("plain application used managed cleanup")
	}
}

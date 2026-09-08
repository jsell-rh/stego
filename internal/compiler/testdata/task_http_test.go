package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestWorkerFailureDrainsHTTPRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	active := make(chan struct{})
	release := make(chan struct{})
	server := stegoHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(active)
		<-release
		w.WriteHeader(http.StatusAccepted)
	}))
	want := errors.New("worker failed")
	done := make(chan error, 1)
	go func() {
		done <- stegoRunTasks(context.Background(), []stegoTask{
			{name: "http", run: func(ctx context.Context) error { return stegoServeHTTP(ctx, listener, server, time.Second) }},
			{name: "worker", run: func(context.Context) error { <-active; return want }},
		})
	}()
	response := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted {
				err = errors.New("response lost")
			}
		}
		response <- err
	}()
	<-active
	select {
	case err := <-done:
		t.Fatalf("returned before request drain: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-response; err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, want) {
		t.Fatalf("lost worker failure: %v", err)
	}
}

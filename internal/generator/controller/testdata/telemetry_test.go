package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	tracing "example.com/records/telemetry"
)

func TestOverlappingControllersShareProviderOwnership(t *testing.T) {
	first, closeFirst, err := startControllerTelemetry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeFirst()
	second, closeSecond, err := startControllerTelemetry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeSecond()
	if first.Value(controllerTelemetryKey{}) != second.Value(controllerTelemetryKey{}) {
		t.Fatal("overlapping controllers created separate metric streams")
	}
	provider := controllerTelemetryOwner.runtime
	detachFirst, err := attachControllerQueue(first, func() QueueMetrics { return QueueMetrics{Capacity: 2} })
	if err != nil {
		t.Fatal(err)
	}
	detachSecond, err := attachControllerQueue(second, func() QueueMetrics { return QueueMetrics{Capacity: 3} })
	if err != nil {
		t.Fatal(err)
	}
	detachFirst()
	closeFirst()
	closeFirst()
	if !provider.LogServiceEvent(context.Background(), tracing.ServiceReady) {
		t.Fatal("first controller closed a shared provider")
	}
	detachSecond()
	closeSecond()
	if provider.LogServiceEvent(context.Background(), tracing.ServiceReady) {
		t.Fatal("last controller did not close its provider")
	}
	if controllerTelemetryOwner.runtime != nil || controllerTelemetryOwner.users != 0 {
		t.Fatal("controller provider ownership was retained")
	}
}

func TestSweepOwnsCommonTelemetryAndPreservesRetryPolicy(t *testing.T) {
	for _, mode := range []string{"failure", "timeout", "canceled", "invalid-page", "peer-canceled"} {
		t.Run(mode, func(t *testing.T) {
			output, err := os.CreateTemp(t.TempDir(), "sweep-logs")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			saved := os.Stderr
			os.Stderr = output
			defer func() { os.Stderr = saved }()
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			reads, actions := 0, 0
			groups := []SweepGroup[string]{{Name: "private-group", Streams: []SweepStream[string]{{Name: "private-stream", Page: func(context.Context, string, int) (SweepPage[string], error) {
				reads++
				if reads == 1 {
					return SweepPage[string]{}, errors.New("private-source-error")
				}
				if mode == "invalid-page" {
					return SweepPage[string]{More: true}, nil
				}
				if mode == "peer-canceled" {
					return SweepPage[string]{Items: []SweepItem[string]{{Cursor: "a", Value: "first"}, {Cursor: "b", Value: "second"}}}, nil
				}
				return SweepPage[string]{Items: []SweepItem[string]{{Cursor: "private-cursor", Value: "private-resource"}}}, nil
			}}}}}
			peerStarted := make(chan struct{})
			err = RunSweep(ctx, groups, func(ctx context.Context, value string) error {
				if mode == "peer-canceled" {
					if value == "first" {
						<-peerStarted
						return denied
					}
					close(peerStarted)
					<-ctx.Done()
					return ctx.Err()
				}
				actions++
				if actions == 1 {
					switch mode {
					case "failure":
						return errors.New("private-provider-error")
					case "timeout":
						<-ctx.Done()
						return ctx.Err()
					case "canceled":
						cancel()
						return ctx.Err()
					}
				}
				if actions == 2 {
					return nil
				}
				return denied
			}, sweepOptions())
			if mode == "invalid-page" {
				if !errors.Is(err, ErrSweepContract) || actions != 0 {
					t.Fatal("invalid page caused an action", err, actions)
				}
			} else if mode == "peer-canceled" {
				if !errors.Is(err, denied) {
					t.Fatal("peer failure was lost", err)
				}
			} else if mode == "canceled" {
				if err != nil || actions != 1 {
					t.Fatal("cancellation changed sweep behavior", err, actions)
				}
			} else if !errors.Is(err, denied) || actions != 3 {
				t.Fatal("retry changed sweep behavior", err, actions)
			}
			if controllerTelemetryOwner.runtime != nil || controllerTelemetryOwner.users != 0 {
				t.Fatal("sweep retained telemetry ownership")
			}
			data, err := os.ReadFile(output.Name())
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte("private-")) {
				t.Fatal("sweep telemetry exposed domain data")
			}
			counts := map[string]int{}
			for _, line := range bytes.Split(data, []byte("\n")) {
				var event struct {
					Event     string `json:"event.name"`
					Operation string `json:"operation"`
					Outcome   string `json:"outcome"`
					Retry     bool   `json:"retry"`
				}
				if json.Unmarshal(line, &event) == nil && event.Event == "controller.work.completed" {
					counts[fmt.Sprintf("%s/%s/%t", event.Operation, event.Outcome, event.Retry)]++
				}
			}
			if counts["scan/failure/true"] != 1 {
				t.Fatal("source retry was not measured", counts)
			}
			switch mode {
			case "invalid-page":
				if counts["scan/failure/false"] != 1 {
					t.Fatal("invalid page was marked retryable", counts)
				}
			case "peer-canceled":
				if counts["reconcile/canceled/false"] != 1 || counts["reconcile/canceled/true"] != 0 || counts["reconcile/failure/false"] != 1 {
					t.Fatal("peer cancellation was marked retryable", counts)
				}
			case "canceled":
				if counts["reconcile/canceled/false"] != 1 {
					t.Fatal("canceled run was marked retryable", counts)
				}
			default:
				if counts["reconcile/"+mode+"/true"] != 1 || counts["reconcile/success/false"] != 1 || counts["reconcile/failure/false"] != 1 {
					t.Fatal("action outcomes or retry policy were lost", counts)
				}
			}
		})
	}
}

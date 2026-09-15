package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	tracing "example.com/records/telemetry"
)

// Formatting a callback error is not part of the telemetry contract.
type privateProcessError struct{}

func (*privateProcessError) Error() string { panic("private-process-error") }

func TestMonitorOwnsSetupAndCleanupTelemetry(t *testing.T) {
	for _, mode := range []string{"success", "failure", "canceled", "panic", "goexit"} {
		t.Run(mode, func(t *testing.T) {
			output, err := os.CreateTemp(t.TempDir(), "process-logs")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			saved := os.Stderr
			os.Stderr = output
			defer func() { os.Stderr = saved }()
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			failure := new(privateProcessError)
			cleaned := false
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = Monitor(ctx, "", func(ctx context.Context, _ *Metrics) error {
				_, done := tracing.TracePostgresOperation(ctx, "server-identity")
				done("success")
				defer func() {
					_, done := tracing.TracePostgresOperation(ctx, "delete")
					done("success")
					cleaned = true
				}()
				// Two completed controller lifetimes must retain the process owner.
				for range 2 {
					child, closeChild, err := startControllerTelemetry(ctx)
					if err != nil {
						return err
					}
					_, finish := beginControllerWork(child, "reconcile")
					finish(nil, false)
					closeChild()
				}
				switch mode {
				case "failure":
					return failure
				case "canceled":
					return context.Canceled
				case "panic":
					panic("private-process-panic")
				case "goexit":
					runtime.Goexit()
				}
				return nil
			})
			if !cleaned {
				t.Fatal("callback cleanup did not finish")
			}
			switch mode {
			case "success":
				if err != nil {
					t.Fatal("successful callback failed")
				}
			case "failure":
				if err != failure {
					t.Fatal("callback error identity was lost")
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) {
					t.Fatal("callback cancellation was lost")
				}
			default:
				if !errors.Is(err, ErrRunAborted) {
					t.Fatal("callback abort was lost")
				}
			}
			body, err := os.ReadFile(output.Name())
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(body, []byte("private-process")) {
				t.Fatal("private callback data entered telemetry")
			}
			counts := map[string]int{}
			instances := map[string]bool{}
			for _, line := range bytes.Split(bytes.TrimSpace(body), []byte{'\n'}) {
				var record map[string]any
				if json.Unmarshal(line, &record) != nil {
					t.Fatal("invalid process event")
				}
				instance, ok := record["service.instance.id"].(string)
				if !ok || instance == "" {
					t.Fatal("process event has no runtime identity")
				}
				instances[instance] = true
				event, _ := record["event.name"].(string)
				counts[event]++
				if event == "postgres.database.completed" {
					counts[event+"/"+record["operation"].(string)]++
				}
			}
			if len(instances) != 1 || counts["telemetry.runtime.started"] != 1 || counts["telemetry.runtime.stopped"] != 1 || counts["controller.work.completed"] != 2 || counts["postgres.database.completed/server-identity"] != 1 || counts["postgres.database.completed/delete"] != 1 {
				t.Fatal("setup, controllers, and cleanup did not share one runtime", counts)
			}
			failed := mode != "success" && mode != "canceled"
			if (counts["service.failed"] == 1) != failed || counts["service.failed"] > 1 || counts["service.ready"] != 0 {
				t.Fatal("incorrect process lifecycle events", counts)
			}
			if controllerTelemetryOwner.runtime != nil || controllerTelemetryOwner.users != 0 {
				t.Fatal("process retained telemetry ownership")
			}
		})
	}
}

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

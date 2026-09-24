package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"
)

// helperTelemetryCapture redirects stderr to a temporary file so local JSON
// log records can be inspected. The test process restores stderr on cleanup.
type helperTelemetryCapture struct{ output *os.File }

func newHelperTelemetryCapture(t *testing.T) *helperTelemetryCapture {
	t.Helper()
	output, err := os.CreateTemp(t.TempDir(), "helper-logs")
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = output
	t.Cleanup(func() {
		os.Stderr = saved
		output.Close()
	})
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	return &helperTelemetryCapture{output: output}
}

// completedRecords returns all controller.work.completed records. Records for
// other operations and non-work events are ignored.
func (c *helperTelemetryCapture) completedRecords(t *testing.T) []map[string]any {
	t.Helper()
	body, err := os.ReadFile(c.output.Name())
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(body), []byte{'\n'}) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record["event.name"] == "controller.work.completed" {
			records = append(records, record)
		}
	}
	return records
}

func (c *helperTelemetryCapture) workOutcomes(t *testing.T, operation string) []string {
	t.Helper()
	var outcomes []string
	for _, record := range c.completedRecords(t) {
		if record["operation"] == operation {
			outcomes = append(outcomes, record["outcome"].(string))
		}
	}
	return outcomes
}

func helperTestSource(after string) func(context.Context, string, int) (CursorPage[string], error) {
	return func(_ context.Context, from string, _ int) (CursorPage[string], error) {
		if from == "a" {
			return CursorPage[string]{}, nil
		}
		return CursorPage[string]{Items: []CursorItem[string]{{"a", "a"}}}, nil
	}
}

func helperTestCheckpointAccess() CheckpointAccess {
	return CheckpointAccess{
		Load: func(context.Context) (Checkpoint, error) { return Checkpoint{}, nil },
		Save: func(context.Context, int64, string) error { return nil },
	}
}

// Each public helper called directly with a telemetry context records exactly
// one controller.work.completed record with the correct operation and outcome.
func TestHelperTelemetryRecordsStandaloneCalls(t *testing.T) {
	capture := newHelperTelemetryCapture(t)
	ctx, closeTelemetry, err := startControllerTelemetry(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	source := helperTestSource("a")
	opts := ScanOptions{PageSize: 2, MaxPages: 1, PageTimeout: time.Second}
	budget := ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}
	access := helperTestCheckpointAccess()
	emit := func(context.Context, string) error { return nil }

	if _, err := ScanFrom(ctx, "", source, func(string) error { return nil }, opts); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("scan emit failed")
	if _, err := ScanFrom(ctx, "", source, func(string) error { return failure }, opts); err == nil {
		t.Fatal("emit failure expected")
	}
	if _, err := ScanCheckpointed(ctx, access, source, emit, opts, budget); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanCycle(ctx, "v1", access, source, emit, nil, opts, budget); err != nil {
		t.Fatal(err)
	}
	if err := RunObservation(ctx, func(context.Context) error { return nil }, func(context.Context, error) error { return nil }, budget); err != nil {
		t.Fatal(err)
	}
	if err := RunObservation(ctx, func(context.Context) error { return failure }, func(context.Context, error) error { return nil }, budget); err == nil {
		t.Fatal("work failure expected")
	}
	closeTelemetry()

	scans := capture.workOutcomes(t, "scan")
	reconciles := capture.workOutcomes(t, "reconcile")
	if fmt.Sprint(scans) != "[success failure success success]" {
		t.Fatal("scan helper outcomes differ", scans)
	}
	if fmt.Sprint(reconciles) != "[success failure]" {
		t.Fatal("reconcile helper outcomes differ", reconciles)
	}
}

// A helper inside an enclosing controller operation records nothing; the
// enclosing operation owns the record.
func TestHelperTelemetrySkipsInsideControllerWork(t *testing.T) {
	capture := newHelperTelemetryCapture(t)
	ctx, closeTelemetry, err := startControllerTelemetry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source := helperTestSource("a")
	opts := ScanOptions{PageSize: 2, MaxPages: 1, PageTimeout: time.Second}

	work, finish := beginControllerWork(ctx, "scan")
	if _, err := ScanFrom(work, "", source, func(string) error { return nil }, opts); err != nil {
		t.Fatal(err)
	}
	finish(nil, false)
	closeTelemetry()

	records := capture.completedRecords(t)
	if len(records) != 1 || records[0]["operation"] != "scan" || records[0]["outcome"] != "success" {
		t.Fatal("nested helper recorded a duplicate operation", records)
	}
}

// ScanCheckpointed delegates to RunObservation and ScanFrom. A standalone call
// records exactly one operation.
func TestHelperTelemetryRecordsOnceAcrossDelegation(t *testing.T) {
	capture := newHelperTelemetryCapture(t)
	ctx, closeTelemetry, err := startControllerTelemetry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source := helperTestSource("a")
	access := helperTestCheckpointAccess()
	opts := ScanOptions{PageSize: 2, MaxPages: 1, PageTimeout: time.Second}
	budget := ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}

	if _, err := ScanCheckpointed(ctx, access, source, func(context.Context, string) error { return nil }, opts, budget); err != nil {
		t.Fatal(err)
	}
	closeTelemetry()

	records := capture.completedRecords(t)
	if len(records) != 1 || records[0]["operation"] != "scan" {
		t.Fatal("delegating helper recorded more than one operation", records)
	}
}

// Standalone ScanStream calls record a scan operation with the correct outcome.
func TestHelperTelemetryRecordsStreamScans(t *testing.T) {
	capture := newHelperTelemetryCapture(t)
	ctx, closeTelemetry, err := startControllerTelemetry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	options := StreamScanOptions{MaxItems: 2, OpenTimeout: time.Second, ReceiveTimeout: time.Second}

	empty := StreamSource[string](func(context.Context) (func() (string, error), error) {
		return func() (string, error) { return "", io.EOF }, nil
	})
	if err := ScanStream(ctx, empty, func(string) error { return nil }, options); err != nil {
		t.Fatal(err)
	}
	blocked := StreamSource[string](func(context.Context) (func() (string, error), error) {
		return func() (string, error) { return "", context.DeadlineExceeded }, nil
	})
	options.ReceiveTimeout = 10 * time.Millisecond
	if err := ScanStream(ctx, blocked, func(string) error { return nil }, options); err == nil {
		t.Fatal("stream timeout expected")
	}
	closeTelemetry()

	scans := capture.workOutcomes(t, "scan")
	if fmt.Sprint(scans) != "[success timeout]" {
		t.Fatal("stream scan outcomes differ", scans)
	}
}

// Contract violations return contract errors and record a failed operation.
func TestHelperTelemetryPreservesContractErrors(t *testing.T) {
	capture := newHelperTelemetryCapture(t)
	ctx, closeTelemetry, err := startControllerTelemetry(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ScanFrom[string](ctx, "", nil, nil, ScanOptions{PageSize: 2, MaxPages: 1, PageTimeout: time.Second}); !errors.Is(err, ErrScanContract) {
		t.Fatal("scan contract error expected", err)
	}
	if err := RunObservation(ctx, nil, nil, ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second}); !errors.Is(err, ErrObservationContract) {
		t.Fatal("observation contract error expected", err)
	}
	closeTelemetry()

	scans := capture.workOutcomes(t, "scan")
	reconciles := capture.workOutcomes(t, "reconcile")
	if fmt.Sprint(scans) != "[failure]" || fmt.Sprint(reconciles) != "[failure]" {
		t.Fatal("contract violations did not record failures", scans, reconciles)
	}
}

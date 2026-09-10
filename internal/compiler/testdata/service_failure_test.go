package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type privateFailure struct{}

func (privateFailure) Error() string { panic("error formatting exposed private data") }

func TestFailureRecordDoesNotFormatCause(t *testing.T) {
	cause := privateFailure{}
	err := &stegoServiceFailure{stage: "database.open", cause: cause}
	if !errors.Is(err, cause) {
		t.Fatal("failure lost its cause")
	}
	var target privateFailure
	if !errors.As(err, &target) {
		t.Fatal("failure lost its type")
	}
	if got := err.Error(); got != "service failed at database.open" {
		t.Fatal("invalid safe error", got)
	}
	for _, input := range []error{err, cause} {
		var output bytes.Buffer
		stegoReportFailure(&output, input)
		var record map[string]any
		if e := json.Unmarshal(output.Bytes(), &record); e != nil {
			t.Fatal(e)
		}
		if len(record) != 5 || record["event.name"] != "service.failed" || record["severity"] != "ERROR" || record["message"] != "Service failed" {
			t.Fatal("invalid failure record", record)
		}
		stage := "service.run"
		if input == err {
			stage = "database.open"
		}
		if record["stage"] != stage {
			t.Fatal("incorrect stage", record)
		}
		if _, e := time.Parse(time.RFC3339Nano, record["timestamp"].(string)); e != nil {
			t.Fatal(e)
		}
	}
}

type blockedFailureOutput struct{ entered, release, done chan struct{} }

func (w *blockedFailureOutput) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	defer close(w.done)
	return len(p), nil
}

func TestFailureOutputHasDeadline(t *testing.T) {
	writer := &blockedFailureOutput{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	returned := make(chan struct{})
	go func() { stegoReportFailure(writer, privateFailure{}); close(returned) }()
	defer func() { close(writer.release); <-writer.done }()
	<-writer.entered
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("failure output blocked exit")
	}
}

type failedFailureOutput struct{ calls int }

func (w *failedFailureOutput) Write([]byte) (int, error) { w.calls++; return 0, io.ErrClosedPipe }
func TestFailureOutputDoesNotRetry(t *testing.T) {
	writer := &failedFailureOutput{}
	stegoReportFailure(writer, privateFailure{})
	if writer.calls != 1 {
		t.Fatal("failure output retried")
	}
}

func TestFailureTaskNamesAreData(t *testing.T) {
	var output bytes.Buffer
	stegoReportFailure(&output, &stegoServiceFailure{stage: "service.run", tasks: []string{"worker[0]"}, cause: privateFailure{}})
	if !strings.Contains(output.String(), `"tasks":["worker[0]"]`) {
		t.Fatal("missing task name")
	}
}

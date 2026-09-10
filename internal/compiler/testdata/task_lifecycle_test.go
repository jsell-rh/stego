package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"example.com/tasks/internal/tasks"
)

func TestTaskFailureStopsService(t *testing.T) {
	t.Setenv("PORT", "0")
	if err := run(); !errors.Is(err, tasks.ErrTask) {
		t.Fatalf("lost task error: %v", err)
	}
	if !tasks.Stopped.Load() || !tasks.Closed.Load() {
		t.Fatal("task stop or resource cleanup was skipped")
	}
}

func TestEarlyReturnFails(t *testing.T) {
	err := stegoRunTasks(context.Background(), []stegoTask{{name: "early", run: func(context.Context) error { return nil }}})
	if err == nil || !strings.Contains(err.Error(), "early") {
		t.Fatalf("early return was not a named failure: %v", err)
	}
}

func TestCancellationWaits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	stopped := make(chan error, 1)
	go func() {
		stopped <- stegoRunTasks(ctx, []stegoTask{{name: "wait", run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			<-release
			return ctx.Err()
		}}})
	}()
	<-started
	cancel()
	select {
	case err := <-stopped:
		t.Fatalf("returned before task cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatalf("normal cancellation failed: %v", err)
	}
}

func TestCanceledStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := stegoRunTasks(ctx, []stegoTask{{name: "unused", run: func(context.Context) error { t.Error("started after cancellation"); return nil }}}); err != nil {
		t.Fatal(err)
	}
}

func TestCancellationKeepsOtherErrors(t *testing.T) {
	want := errors.New("cleanup failed")
	ctx, cancel := context.WithCancel(context.Background())
	err := stegoRunTasks(ctx, []stegoTask{{name: "cleanup", run: func(context.Context) error {
		cancel()
		return errors.Join(context.Canceled, want)
	}}})
	if !errors.Is(err, want) {
		t.Fatalf("lost cleanup error: %v", err)
	}
}

func TestTaskSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process signal test")
	}
	binary := filepath.Join(t.TempDir(), "service")
	build := exec.Command("go", "build", "-race", "-mod=readonly", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	marker := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(binary)
	command.Env = append(os.Environ(), "TASK_SIGNAL=1", "TASK_READY="+marker, "PORT=0")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() { _ = command.Process.Kill() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("exited before readiness: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("task did not become ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("signal shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("signal shutdown did not finish")
	}
}

func TestTaskFailureNamesExcludeCauses(t *testing.T) {
	first := errors.New("private-task-cause")
	err := stegoRunTasks(context.Background(), []stegoTask{
		{name: "worker-b", run: func(context.Context) error { return first }},
		{name: "worker-a", run: func(context.Context) error { return first }},
	})
	if !errors.Is(err, first) {
		t.Fatal("lost task cause")
	}
	if strings.Contains(err.Error(), "private-task-cause") {
		t.Fatal("task summary exposed its cause")
	}
	names := stegoTaskNames(err)
	if len(names) != 2 || names[0] != "worker-a" || names[1] != "worker-b" {
		t.Fatal("invalid task summary", names)
	}
}

type privateTaskPanic struct{}

func (privateTaskPanic) Error() string { panic("private panic formatter must not run") }

func TestAbnormalTaskExitStopsAndJoinsPeers(t *testing.T) {
	cases := []struct {
		name  string
		abort func()
	}{
		{"panic", func() { panic("private-task-panic") }},
		{"nil panic", func() { panic(nil) }},
		{"error panic", func() { panic(privateTaskPanic{}) }},
		{"deferred panic", func() { defer func() { panic(privateTaskPanic{}) }() }},
		{"Goexit", runtime.Goexit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			started, left, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- stegoRunTasks(context.Background(), []stegoTask{
					{name: "broken", run: func(context.Context) error { <-started; defer close(left); tc.abort(); return nil }},
					{name: "peer", run: func(ctx context.Context) error {
						close(started)
						<-ctx.Done()
						select {
						case <-left:
						default:
							t.Error("peer canceled before task defers finished")
						}
						close(canceled)
						<-release
						return ctx.Err()
					}},
				})
			}()
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("abnormal task exit did not cancel peer")
			}
			select {
			case <-done:
				t.Fatal("supervisor returned before peer cleanup")
			case <-time.After(10 * time.Millisecond):
			}
			close(release)
			select {
			case err := <-done:
				if err == nil || strings.Contains(err.Error(), "private") {
					t.Fatal("abnormal exit lost failure or exposed private data")
				}
				if names := stegoTaskNames(err); len(names) != 1 || names[0] != "broken" {
					t.Fatal("task name was lost", names)
				}
				if names := stegoAbortedTaskNames(err); len(names) != 1 || names[0] != "broken" {
					t.Fatal("abort classification was lost", names)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("supervisor did not join tasks")
			}
		})
	}
}

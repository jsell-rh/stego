//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly || windows

package compiler

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentApplyHasOneWriter(t *testing.T) {
	input := snapshotTestInput(t)
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			results <- Apply(plan, input.ProjectDir, "")
		})
	}
	close(start)
	workers.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("concurrent writers = %d, want 1", succeeded)
	}
	drift, err := DetectDrift(input.ProjectDir, filepath.Join(input.ProjectDir, "out"))
	if err != nil || drift.HasDrift() {
		t.Fatalf("concurrent apply left mixed output: %v", err)
	}
}

func TestProjectLockIsReleasedWhenProcessDies(t *testing.T) {
	project := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestLockProcessHelper$")
	command.Env = append(os.Environ(), "STEGO_TEST_LOCK_PROJECT="+project)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("child failed to acquire lock: %q, %v", line, err)
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if lock, err := lockProject(root); err == nil {
		lock.Close()
		t.Fatal("another process acquired the same lock")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("child did not terminate with an error")
	}
	lock, err := lockProject(root)
	if err != nil {
		t.Fatalf("dead process retained its lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project, ".stego/apply.lock")); err != nil {
		t.Fatalf("lock inode was removed: %v", err)
	}
}

func TestLockProcessHelper(t *testing.T) {
	project := os.Getenv("STEGO_TEST_LOCK_PROJECT")
	if project == "" {
		return
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := lockProject(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	fmt.Println("locked")
	_, _ = os.Stdin.Read(make([]byte, 1))
}

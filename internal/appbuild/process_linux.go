package appbuild

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func openInput(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

type boundedWriter struct {
	destination io.Writer
	left        int64
	cancel      context.CancelFunc
}

func (w *boundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.left {
		w.cancel()
		return 0, errors.New("build command output exceeds its limit")
	}
	n, err := w.destination.Write(data)
	w.left -= int64(n)
	return n, err
}

func command(ctx context.Context, directory string, environment []string, output io.Writer, limit int64, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir, cmd.Env = directory, environment
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	// A child can keep output pipes open after the direct process exits.
	// End the remaining process group before returning from either path.
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	cmd.Stdout = &boundedWriter{output, limit, cancel}
	// Diagnostics can contain local paths or private dependency information.
	cmd.Stderr = &boundedWriter{io.Discard, 1 << 20, cancel}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("application build command failed; private diagnostics are withheld")
	}
	return ctx.Err()
}

func capture(ctx context.Context, directory string, environment []string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	err := command(ctx, directory, environment, &out, 8<<20, args...)
	return out.Bytes(), err
}

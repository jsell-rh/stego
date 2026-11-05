//go:build stegotesthook && unix

package compiler

import (
	"os"
	"syscall"
)

// A test build of stego (go build -tags stegotesthook) stops apply with
// SIGKILL at the journal point that STEGO_TEST_STOP_APPLY names, and appends
// each point it passes, one per line, to the file that STEGO_TEST_APPLY_LOG
// names. A product build holds no part of this file.
func init() {
	stopHook = stopAtJournalPoint
}

func stopAtJournalPoint(point string) error {
	if log := os.Getenv("STEGO_TEST_APPLY_LOG"); log != "" {
		file, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = file.WriteString(point + "\n")
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	if os.Getenv("STEGO_TEST_STOP_APPLY") == point {
		return syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
	return nil
}

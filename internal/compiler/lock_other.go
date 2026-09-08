//go:build !(linux || darwin || freebsd || openbsd || netbsd || dragonfly || windows)

package compiler

import (
	"fmt"
	"os"
	"runtime"
)

func lockFile(file *os.File) error {
	return fmt.Errorf("apply does not support process locking on %s", runtime.GOOS)
}

func unlockFile(file *os.File) error { return nil }

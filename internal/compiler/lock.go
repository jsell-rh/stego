package compiler

import (
	"errors"
	"fmt"
	"os"
)

// lockProject holds a process lock until the returned lock is closed. The lock
// file stays in place so all processes lock the same file.
func lockProject(root *os.Root) (*projectLock, error) {
	const name = ".stego/apply.lock"
	if err := checkFileTarget(root, name); err != nil {
		return nil, err
	}
	if err := root.MkdirAll(".stego", 0755); err != nil {
		return nil, err
	}
	file, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("cannot lock project; another STEGO process may be active: %w", err)
	}
	return &projectLock{file: file}, nil
}

type projectLock struct{ file *os.File }

func (lock *projectLock) Close() error {
	return errors.Join(unlockFile(lock.file), lock.file.Close())
}

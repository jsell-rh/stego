//go:build !linux

package appbuild

import (
	"context"
	"errors"
)

// keptTree is not kept on a system other than Linux, where application
// builds do not run.
type keptTree struct{}

func openKeptTree(context.Context, string, string) (*keptTree, error) {
	return nil, errors.New("the kept tree needs Linux")
}

func (k *keptTree) close()                      {}
func (k *keptTree) read(context.Context) []byte { return nil }
func (k *keptTree) write([]byte) error          { return nil }

//go:build !linux

package appbuild

import (
	"context"
	"errors"
	"io"
	"os"
)

func openInput(name string) (*os.File, error) { return os.Open(name) }

func command(context.Context, string, []string, io.Writer, int64, ...string) error {
	return errors.New("application builds require Linux")
}
func capture(context.Context, string, []string, ...string) ([]byte, error) {
	return nil, errors.New("application builds require Linux")
}

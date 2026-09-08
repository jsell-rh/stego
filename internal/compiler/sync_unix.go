//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package compiler

import "os"

func syncDirectory(root *os.Root, name string) error {
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

//go:build !(linux || darwin || freebsd || openbsd || netbsd || dragonfly)

package compiler

import "os"

// File contents are synced on all supported systems. Directory metadata sync
// is available through the Unix implementation. Power-loss recovery on other
// filesystems requires separate platform testing.
func syncDirectory(root *os.Root, name string) error { return nil }

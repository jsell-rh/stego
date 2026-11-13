//go:build linux

package appbuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

// keptTree is the latest signed tree of a sum database, kept in a folder of
// the current user between runs, which a lookup holds under the lock of that
// folder from its first read to its last write.
type keptTree struct {
	folder string
	key    string
	lock   *os.File
}

// openKeptTree takes the lock of the folder, which it makes when it is
// missing. It refuses a folder that is a link, that is not a folder, that
// another user owns or that its group or others may write, so that no other
// user can plant or change the kept tree.
func openKeptTree(ctx context.Context, folder, key string) (*keptTree, error) {
	if err := os.MkdirAll(filepath.Dir(folder), 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(folder, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := os.Lstat(folder)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", folder)
	}
	if err := ownedAlone(info); err != nil {
		return nil, fmt.Errorf("%s: %w", folder, err)
	}
	lock, err := os.OpenFile(filepath.Join(folder, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	locked := make(chan error, 1)
	go func() { locked <- syscall.Flock(int(lock.Fd()), syscall.LOCK_EX) }()
	select {
	case err := <-locked:
		if err != nil {
			lock.Close()
			return nil, err
		}
	case <-ctx.Done():
		// Closing the file ends the wait for its lock.
		lock.Close()
		return nil, ctx.Err()
	}
	return &keptTree{folder: folder, key: key, lock: lock}, nil
}

// ownedAlone refuses a file of another user, or one that its group or
// others may write.
func ownedAlone(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("the owner cannot be read")
	}
	if int(stat.Uid) != os.Getuid() {
		return errors.New("another user owns it")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("its group or others may write it")
	}
	return nil
}

func (k *keptTree) close() {
	_ = syscall.Flock(int(k.lock.Fd()), syscall.LOCK_UN)
	_ = k.lock.Close()
}

// read gives the kept tree, or nothing when there is none. A kept tree that
// is a link, not a regular file, of another user, writable by others, larger
// than maxSignedTree or not a tree signed by the key of the database is
// removed with a warning, and the lookup starts from no tree, which the run
// then keeps again: a bad file never ends the check of the database.
func (k *keptTree) read(ctx context.Context) []byte {
	path := filepath.Join(k.folder, "latest")
	data, err := k.load(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		slog.WarnContext(ctx, "the kept tree of the sum database is not valid; it is removed and kept again from this run", "file", path, "error", err)
		_ = os.Remove(path)
		return nil
	}
	return data
}

func (k *keptTree) load(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("it is not a regular file")
	}
	if err := ownedAlone(info); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSignedTree+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSignedTree {
		return nil, fmt.Errorf("it is larger than %d bytes", maxSignedTree)
	}
	verifier, err := note.NewVerifier(k.key)
	if err != nil {
		return nil, err
	}
	signed, err := note.Open(data, note.VerifierList(verifier))
	if err != nil {
		return nil, fmt.Errorf("it is not a note signed by the key of the database: %w", err)
	}
	if _, err := tlog.ParseTree([]byte(signed.Text)); err != nil {
		return nil, fmt.Errorf("it is not a tree: %w", err)
	}
	return data, nil
}

// write keeps the tree in a new file that it renames over the old one, so
// a reader sees one whole tree or the one before it.
func (k *keptTree) write(data []byte) error {
	temporary, err := os.CreateTemp(k.folder, ".latest-*")
	if err != nil {
		return err
	}
	_, err = temporary.Write(data)
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary.Name(), filepath.Join(k.folder, "latest"))
	}
	if err != nil {
		_ = os.Remove(temporary.Name())
	}
	return err
}

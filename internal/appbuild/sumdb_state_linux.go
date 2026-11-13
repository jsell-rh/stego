//go:build linux

package appbuild

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

// keptTree is the latest signed tree of a sum database, kept in a folder of
// the current user between runs, which a lookup holds under the lock of that
// folder from its first read to its last write. Every file of the folder is
// reached through one handle of the folder (openat, renameat, unlinkat), so
// a change of the path above it after the folder is opened cannot send a
// read or a write elsewhere.
type keptTree struct {
	folder string
	key    string
	dir    *os.File
	lock   *os.File
}

// lockPoll is how often a lookup tries again to take the lock of the folder.
const lockPoll = 20 * time.Millisecond

// openKeptTree opens the folder, which it makes when it is missing, as one
// handle that does not follow a link, and takes the lock of the folder. It
// refuses a folder that is a link, that is not a folder, that another user
// owns or that its group or others may write, so that no other user can
// plant or change the kept tree. It waits for the lock no longer than ctx
// allows, and leaves nothing running when it gives up.
func openKeptTree(ctx context.Context, folder, key string) (*keptTree, error) {
	if err := os.MkdirAll(filepath.Dir(folder), 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(folder, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	fd, err := syscall.Open(folder, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
			return nil, fmt.Errorf("%s is not a folder", folder)
		}
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), folder)
	info, err := dir.Stat()
	if err == nil {
		err = ownedAlone(info)
	}
	if err != nil {
		dir.Close()
		return nil, fmt.Errorf("%s: %w", folder, err)
	}
	k := &keptTree{folder: folder, key: key, dir: dir}
	lock, err := k.openAt("lock", syscall.O_CREAT|syscall.O_RDWR, 0o600)
	if err != nil {
		dir.Close()
		return nil, err
	}
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			lock.Close()
			dir.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			lock.Close()
			dir.Close()
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
	k.lock = lock
	return k, nil
}

// openAt opens the file name of the folder through its handle, never
// through a link.
func (k *keptTree) openAt(name string, flags int, mode uint32) (*os.File, error) {
	fd, err := syscall.Openat(int(k.dir.Fd()), name, flags|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, mode)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: filepath.Join(k.folder, name), Err: err}
	}
	return os.NewFile(uintptr(fd), filepath.Join(k.folder, name)), nil
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
	_ = k.dir.Close()
}

// read gives the kept tree, or nothing when there is none. A kept tree that
// is a link, not a regular file, of another user, writable by others, larger
// than maxSignedTree or not a tree signed by the key of the database is
// removed with a warning, and the lookup starts from no tree, which the run
// then keeps again: a bad file never ends the check of the database.
func (k *keptTree) read(ctx context.Context) []byte {
	data, err := k.load()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		slog.WarnContext(ctx, "the kept tree of the sum database is not valid; it is removed and kept again from this run",
			"file", filepath.Join(k.folder, "latest"), "error", err)
		_ = syscall.Unlinkat(int(k.dir.Fd()), "latest")
		return nil
	}
	return data
}

func (k *keptTree) load() ([]byte, error) {
	file, err := k.openAt("latest", syscall.O_RDONLY, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, os.ErrNotExist
	}
	if errors.Is(err, syscall.ELOOP) {
		return nil, errors.New("it is a link")
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("it is not a regular file")
	}
	if err := ownedAlone(info); err != nil {
		return nil, err
	}
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

// write keeps the tree in a new file of the folder that it renames over the
// old one, both through the handle of the folder, so a reader sees one whole
// tree or the one before it.
func (k *keptTree) write(data []byte) error {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	temporary := ".latest-" + hex.EncodeToString(suffix)
	file, err := k.openAt(temporary, syscall.O_CREAT|syscall.O_EXCL|syscall.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = syscall.Renameat(int(k.dir.Fd()), temporary, int(k.dir.Fd()), "latest")
	}
	if err != nil {
		_ = syscall.Unlinkat(int(k.dir.Fd()), temporary)
	}
	return err
}

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

// openKeptTree opens the folder through walkKeptFolder, which makes it when
// it is missing, as one handle that does not follow a link, and takes the
// lock of the folder. It
// refuses a folder that is a link, that is not a folder, that another user
// owns or that its group or others may write, so that no other user can
// plant or change the kept tree. It waits for the lock no longer than ctx
// allows, and leaves nothing running when it gives up.
func openKeptTree(ctx context.Context, folder, key string) (*keptTree, error) {
	dir, err := walkKeptFolder(folder)
	if err != nil {
		return nil, err
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

// walkKeptFolder opens the folder of the kept tree from its grandparent, the
// cache root such as ~/.cache, which it opens as a folder, through the
// parent such as stego and then the folder such as sumdb, each with openat
// and O_DIRECTORY|O_NOFOLLOW from the handle of the step before, and makes
// each with mkdirat (mode 0700) when it is missing. The cache root must be
// the user's own folder that its group and others may not write, or a folder
// of root with the sticky bit, and each of the two must be a folder, not a
// link, owned by the user and not writable by its group or others; else the
// kept tree is not used. No path is followed below the
// cache root, so a link at the parent cannot send the tree elsewhere.
func walkKeptFolder(folder string) (*os.File, error) {
	root := filepath.Dir(filepath.Dir(folder))
	// The cache root is the user's own choice, such as XDG_CACHE_HOME, and may be a link; below it nothing is.
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	// A cache root that is a link must be the user's own link (#259): on a system with fs.protected_symlinks=0, the
	// kernel follows a link that another user put in a shared folder such as /tmp, and that link could send the kept
	// tree to a folder of that user.
	info, err := lstatRoot(root)
	if err == nil {
		err = rootLinkSafe(info)
	}
	if err != nil {
		return nil, fmt.Errorf("the cache root %s: %w", root, err)
	}
	// A cache root that is no link is opened with O_NOFOLLOW, so a link that another user puts in its place after
	// the Lstat gives ELOOP (or ENOTDIR, which Linux gives first for O_DIRECTORY) and is not followed. The user's own link is followed; the folder that it names is
	// checked when it is open.
	flags := syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_CLOEXEC
	if info.Mode()&os.ModeSymlink == 0 {
		flags |= syscall.O_NOFOLLOW
	}
	fd, err := syscall.Open(root, flags, 0)
	if err != nil {
		if flags&syscall.O_NOFOLLOW != 0 && (errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR)) {
			return nil, fmt.Errorf("the cache root %s: it became a link or no folder after it was read", root)
		}
		return nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	current := os.NewFile(uintptr(fd), root)
	// The root is a folder of the user's that its group and others may not write, or a shared folder of root with
	// the sticky bit, such as /tmp, where no other user can move or replace what the user made in it; else another
	// user could replace the parent before it is opened, and the kept tree is not used.
	if info, err := current.Stat(); err != nil || cacheRootSafe(info) != nil {
		current.Close()
		if err == nil {
			err = cacheRootSafe(info)
		}
		return nil, fmt.Errorf("the cache root %s: %w", root, err)
	}
	path := root
	for _, name := range []string{filepath.Base(filepath.Dir(folder)), filepath.Base(folder)} {
		path = filepath.Join(path, name)
		if err := syscall.Mkdirat(int(current.Fd()), name, 0o700); err != nil && !errors.Is(err, syscall.EEXIST) {
			current.Close()
			return nil, &os.PathError{Op: "mkdir", Path: path, Err: err}
		}
		next, err := syscall.Openat(int(current.Fd()), name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		current.Close()
		if err != nil {
			if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
				return nil, fmt.Errorf("%s is not a folder", path)
			}
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		current = os.NewFile(uintptr(next), path)
		info, err := current.Stat()
		if err == nil {
			err = ownedAlone(info)
		}
		if err != nil {
			current.Close()
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return current, nil
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

// cacheRootSafe refuses a cache root that is not the user's own folder that its group and others may not write,
// nor a folder of root with the sticky bit.
func cacheRootSafe(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("the owner cannot be read")
	}
	if stat.Uid == 0 && info.Mode()&os.ModeSticky != 0 {
		return nil
	}
	return ownedAlone(info)
}

// lstatRoot reads the cache root without following a link. A test replaces it to give an owner that it cannot set
// up, or an entry that changes after it is read.
var lstatRoot = os.Lstat

// rootLinkSafe refuses a cache root that is a link of another user. A cache root that is no link, or the user's
// own link, passes; the folder that it names is checked when it is open.
func rootLinkSafe(info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("the owner of the link cannot be read")
	}
	if int(stat.Uid) != os.Getuid() {
		return errors.New("it is a link of another user")
	}
	return nil
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

package appbuild

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSourceTreeRejectsLinksAndMalformedEntries(t *testing.T) {
	object := strings.Repeat("a", 40)
	for _, entry := range []string{
		"", "100644 blob " + object + "\tfile", "100644 blob " + object + "\t../file\x00",
		"120000 blob " + object + "\tlink\x00", "160000 commit " + object + "\tmodule\x00",
		"100644 blob wrong\tfile\x00", "100644 blob " + object + "\tfile\x00\x00",
		strings.Repeat("100644 blob "+object+"\tfile\x00", 2),
	} {
		if _, err := sourceTree([]byte(entry)); err == nil {
			t.Fatal("invalid source tree was accepted")
		}
	}
}

func TestSourceArchiveMustMatchRawGitObjects(t *testing.T) {
	for _, change := range []string{"none", "content", "mode", "missing", "extra"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, "file", "hello\n")
			// This is Git's object name for the exact six-byte content.
			entries, err := sourceTree([]byte("100644 blob ce013625030ba8dba906f756967f9e9ca394464a\tfile\x00"))
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "content":
				put(t, root, "file", "other\n")
			case "mode":
				if err := os.Chmod(filepath.Join(root, "file"), 0700); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(filepath.Join(root, "file")); err != nil {
					t.Fatal(err)
				}
			case "extra":
				put(t, root, "extra", "not selected")
			}
			_, files, err := inventory(root, maxFiles, maxBytes)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifySourceTree(root, entries, files); (err == nil) != (change == "none") {
				t.Fatal("unexpected source comparison", err)
			}
		})
	}
}

func TestGitExportAttributesCannotChangeSelectedSource(t *testing.T) {
	for _, kind := range []string{"none", "committed omit", "local omit", "committed substitute"} {
		t.Run(kind, func(t *testing.T) {
			repo := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			run := func(args ...string) []byte {
				cmd := exec.CommandContext(ctx, "/usr/bin/git", args...)
				cmd.Dir = repo
				cmd.Env = []string{"HOME=" + repo, "PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1"}
				out, err := cmd.Output()
				if err != nil {
					t.Fatal("Git source fixture failed", err)
				}
				return out
			}
			run("init", "--quiet", "--object-format=sha1")
			put(t, repo, "file", "$Format:%H$\n")
			switch kind {
			case "committed omit":
				put(t, repo, ".gitattributes", "file export-ignore\n")
			case "local omit":
				put(t, repo, ".git/info/attributes", "file export-ignore\n")
			case "committed substitute":
				put(t, repo, ".gitattributes", "file export-subst\n")
			}
			run("add", ".")
			run("-c", "user.name=Source Test", "-c", "user.email=source@example.test", "-c", "commit.gpgsign=false", "commit", "-qm", "Source fixture")
			entries, err := sourceTree(run("ls-tree", "-r", "-z", "HEAD"))
			if err != nil {
				t.Fatal(err)
			}
			snapshot := t.TempDir()
			if err := unpack(bytes.NewReader(run("archive", "--format=tar", "HEAD")), snapshot); err != nil {
				t.Fatal(err)
			}
			_, files, err := inventory(snapshot, maxFiles, maxBytes)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifySourceTree(snapshot, entries, files); (err == nil) != (kind == "none") {
				t.Fatal("unexpected export attribute result", err)
			}
		})
	}
}

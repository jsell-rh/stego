package appbuild

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestImageTransportRetainsVerifiedContentAndStableBytes(t *testing.T) {
	root, record := imageFixture(t, imageTestCA(t, true))
	first, err := exportImageArchive(context.Background(), filepath.Join(root, "oci"), record, filepath.Join(root, "first.tar"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := exportImageArchive(context.Background(), filepath.Join(root, "oci"), record, filepath.Join(root, "second.tar"))
	if err != nil || first != second {
		t.Fatal("transport changed across paths", err)
	}
	if _, err := exportImageArchive(context.Background(), filepath.Join(root, "oci"), record, filepath.Join(root, "first.tar")); err == nil {
		t.Fatal("existing archive was replaced")
	}
	if err := os.WriteFile(filepath.Join(root, "oci/blobs/sha256", record.Config.SHA256), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := exportImageArchive(context.Background(), filepath.Join(root, "oci"), record, filepath.Join(root, "unverified.tar")); err == nil {
		t.Fatal("changed image was exported")
	}
	if _, err := os.Lstat(filepath.Join(root, "unverified.tar")); !os.IsNotExist(err) {
		t.Fatal("unverified output was written", err)
	}
}

package compiler

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// storeMarks are the marks of the GORM store path in source code: the GORM store and its models, AutoMigrate, the
// storage and events contracts that the store served, the data path of the record and its switch, the store path
// test registry, the tsl-search component, and the refusal of a ledger row of a GORM release (ST-SUP-RC20-OC39).
var storeMarks = map[string]*regexp.Regexp{
	"AutoMigrate":                 regexp.MustCompile(`AutoMigrate`),
	"a GORM model tag":            regexp.MustCompile("gorm:\""),
	"gorm.DeletedAt":              regexp.MustCompile(`gorm\.DeletedAt`),
	"the GORM store constructor":  regexp.MustCompile(`storage\.NewStore\(|\*storage\.Store\b|stegostorage\.Store\b`),
	"the storage contract":        regexp.MustCompile(`\bStorageContract\b|\bStorageV1\b|\bEventsContract\b|\bEventsV1\b`),
	"the data path of the record": regexp.MustCompile(`\bDataPath(Store|Runtime)?\b|yaml:"data_path`),
	"the store path registry":     regexp.MustCompile(`\bstoreRegistry\w*\(|\bstoreArchetypes\b`),
	"tsl-search":                  regexp.MustCompile(`generator/tslsearch|"tsl-search"`),
	"a GORM ledger row":           regexp.MustCompile(`release on the GORM store|legacyDigest|upgradeLedger`),
}

// TestStoreRemoved is the proof of the removal of the GORM store path.
func TestStoreRemoved(t *testing.T) {
	t.Run("ST-SUP-RC20-OC39-T01 Once the trigger holds, the tree holds no store path, data_path switch or GORM ledger protection", func(t *testing.T) {
		root := filepath.Join("..", "..")
		for _, folder := range []string{"internal", "cmd", "registry"} {
			err := filepath.WalkDir(filepath.Join(root, folder), func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				name := filepath.ToSlash(path)
				if entry.IsDir() {
					// The runtime library is a module of its own, and the assure module reads git objects of its own.
					if strings.HasSuffix(name, "/registry/runtime") || strings.HasSuffix(name, "/internal/assure") || entry.Name() == "testdata" {
						return filepath.SkipDir
					}
					return nil
				}
				if strings.HasSuffix(name, "_test.go") || !(strings.HasSuffix(name, ".go") || strings.HasSuffix(name, ".tmpl")) {
					return nil
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				for mark, pattern := range storeMarks {
					// The reader of a record of format version 4 names the key data_path to remove it, and only there.
					if mark == "the data path of the record" && name == "../../internal/compiler/state.go" {
						data = []byte(strings.Replace(string(data), "yaml:\"data_path,omitempty\"", "", 1))
					}
					if pattern.Match(data) {
						t.Errorf("%s holds %s", name, mark)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "registry", "components", "tsl-search")); !os.IsNotExist(err) {
			t.Errorf("the registry holds the component tsl-search: %v", err)
		}

		// Each example is generated on the runtime data path: its record is of format version 5, with no data path
		// and no file of the GORM store. ST-SUP-RC01-OC41-T06 generates each example again and expects no change.
		for _, example := range []string{"user-management", "user-management-rhsso"} {
			record, err := os.ReadFile(filepath.Join(root, "examples", example, ".stego", "state.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			text := string(record)
			if !strings.HasPrefix(text, "format_version: 5\n") || strings.Contains(text, "data_path") {
				t.Errorf("the record of the example %s is not of format version 5 without a data path", example)
			}
			for _, file := range []string{"internal/storage/store.go", "internal/storage/models.go", "contracts/storage/"} {
				if strings.Contains(text, file) {
					t.Errorf("the record of the example %s names %s of the GORM store", example, file)
				}
			}
		}
	})
}

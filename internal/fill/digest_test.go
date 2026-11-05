package fill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// untestedFillFiles are the files of untested-fill of ST-EXT-TD01: code and a
// passing test for rest-api.before_create and org-users, with no evidence.
func untestedFillFiles() map[string]string {
	return map[string]string{
		"fill.yaml":      "kind: fill\nname: untested-fill\nimplements: rest-api.before_create\ncollection: org-users\n",
		"policy.go":      "package untested_fill\n\nimport (\n\t\"context\"\n\n\tslots \"example.com/fills/out/slots\"\n)\n\n// Policy allows each creation.\ntype Policy struct{}\n\n// New makes the policy.\nfunc New() *Policy { return &Policy{} }\n\n// Evaluate allows the creation.\nfunc (p *Policy) Evaluate(ctx context.Context, req *slots.BeforeCreateRequest) (*slots.SlotResult, error) {\n\treturn &slots.SlotResult{Ok: true}, nil\n}\n",
		"policy_test.go": "package untested_fill\n\nimport (\n\t\"context\"\n\t\"testing\"\n\n\tslots \"example.com/fills/out/slots\"\n)\n\nfunc TestPolicyAllows(t *testing.T) {\n\tresult, err := New().Evaluate(context.Background(), &slots.BeforeCreateRequest{})\n\tif err != nil || result == nil || !result.Ok {\n\t\tt.Fatalf(\"the policy denied: %v\", err)\n\t}\n}\n",
	}
}

// writeFiles writes the files into a new folder, in the order of names, and
// gives the folder.
func writeFiles(t *testing.T, files map[string]string, names []string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(files[name]), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func sortedNames(files map[string]string, reverse bool) []string {
	var names []string
	for name := range files {
		names = append(names, name)
	}
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if (names[j] < names[i]) != reverse {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}

func mustDigest(t *testing.T, dir string) string {
	t.Helper()
	digest, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestFillDigest(t *testing.T) {
	t.Run("ST-EXT-DS01-T01 The fill digest is the same for the same files, in whatever order they are written", func(t *testing.T) {
		files := untestedFillFiles()
		first := writeFiles(t, files, sortedNames(files, false))
		second := writeFiles(t, files, sortedNames(files, true))
		old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
		for name := range files {
			if err := os.Chtimes(filepath.Join(second, name), old, old); err != nil {
				t.Fatal(err)
			}
		}
		digests := []string{mustDigest(t, first), mustDigest(t, first), mustDigest(t, second), mustDigest(t, second)}
		for _, digest := range digests[1:] {
			if digest != digests[0] {
				t.Fatalf("the digests differ: %v", digests)
			}
		}
		if !validDigest(digests[0]) {
			t.Fatalf("the digest %q is not 64 lower-case hexadecimal digits", digests[0])
		}
	})

	t.Run("ST-EXT-DS01-T02 A change to any file of the fill folder or to a file name changes the fill digest", func(t *testing.T) {
		files := untestedFillFiles()
		first := mustDigest(t, writeFiles(t, files, sortedNames(files, false)))
		changes := map[string]func(map[string]string){
			"one byte of policy.go": func(f map[string]string) {
				f["policy.go"] = strings.Replace(f["policy.go"], "allows each", "allowz each", 1)
			},
			"one byte of policy_test.go": func(f map[string]string) {
				f["policy_test.go"] = strings.Replace(f["policy_test.go"], "the policy denied", "the policy denieD", 1)
			},
			"a new file README.md": func(f map[string]string) { f["README.md"] = "# untested-fill\n" },
			"policy_test.go renamed to policy_check.go": func(f map[string]string) {
				f["policy_check.go"] = f["policy_test.go"]
				delete(f, "policy_test.go")
			},
			"the collection set to org-groups": func(f map[string]string) {
				f["fill.yaml"] = strings.Replace(f["fill.yaml"], "org-users", "org-groups", 1)
			},
		}
		seen := map[string]string{first: "the first digest"}
		for name, change := range changes {
			files := untestedFillFiles()
			change(files)
			digest := mustDigest(t, writeFiles(t, files, sortedNames(files, false)))
			if other, ok := seen[digest]; ok {
				t.Errorf("%s gives the same digest as %s", name, other)
			}
			seen[digest] = name
		}
	})

	t.Run("ST-EXT-DS01-T03 Recording evidence or a sign-off in the fill manifest does not change the fill digest", func(t *testing.T) {
		files := untestedFillFiles()
		dir := writeFiles(t, files, sortedNames(files, false))
		manifest := filepath.Join(dir, ManifestFile)
		first := mustDigest(t, dir)
		evidence := FillEvidence{FillDigest: strings.Repeat("a", 64), ContractDigest: strings.Repeat("b", 64), PassedAt: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), Files: map[string]string{"policy.go": strings.Repeat("c", 64)}}
		if err := WriteEvidence(manifest, evidence); err != nil {
			t.Fatal(err)
		}
		withEvidence := mustDigest(t, dir)
		signOff := FillSignOff{EvidenceDigest: strings.Repeat("d", 64), Signature: Signature{KeyType: KeySSH, Signer: "listed@example.com", Armored: "-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"}, SignedAt: time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)}
		if err := WriteSignOff(manifest, signOff); err != nil {
			t.Fatal(err)
		}
		withSignOff := mustDigest(t, dir)
		evidence.FillDigest, evidence.PassedAt = strings.Repeat("e", 64), evidence.PassedAt.Add(time.Hour)
		signOff.EvidenceDigest, signOff.Signature.Signer = strings.Repeat("f", 64), "other@example.com"
		if err := WriteEvidence(manifest, evidence); err != nil {
			t.Fatal(err)
		}
		if err := WriteSignOff(manifest, signOff); err != nil {
			t.Fatal(err)
		}
		changed := mustDigest(t, dir)
		data, _ := os.ReadFile(manifest)
		if !strings.Contains(string(data), strings.Repeat("e", 64)) || !strings.Contains(string(data), strings.Repeat("f", 64)) {
			t.Fatalf("the manifest does not hold the changed evidence and sign-off:\n%s", data)
		}
		for _, digest := range []string{withEvidence, withSignOff, changed} {
			if digest != first {
				t.Fatalf("the digests differ: %s %s %s %s", first, withEvidence, withSignOff, changed)
			}
		}
	})

	t.Run("ST-EXT-DS01-T04 The contract digest changes with the slot's .proto file and with each file it imports, and with no other file of the component", func(t *testing.T) {
		registry := copyRegistry(t)
		contracts := dirContracts(registry)
		contract := "components/rest-api/slots/before_create.proto"
		first := mustContractDigest(t, contracts, contract)
		restore := changeComment(t, filepath.Join(registry, contract))
		second := mustContractDigest(t, contracts, contract)
		restore()
		restore = changeComment(t, filepath.Join(registry, "common/types.proto"))
		third := mustContractDigest(t, contracts, contract)
		restore()
		changeComment(t, filepath.Join(registry, "components/rest-api/slots/before_delete.proto"))
		appendLine(t, filepath.Join(registry, "components/rest-api/component.yaml"), "# a change\n")
		fourth := mustContractDigest(t, contracts, contract)
		if second == first || third == first || second == third {
			t.Fatalf("a change to the contract or to its import leaves the digest: %s %s %s", first, second, third)
		}
		if fourth != first {
			t.Fatalf("a change to another file of the component changes the digest: %s %s", first, fourth)
		}
	})

	t.Run("ST-EXT-DS01-T05 The contract digest covers a file that the slot's .proto file imports only through another import", func(t *testing.T) {
		files := map[string]string{
			"components/hooks/slots/hook.proto": "syntax = \"proto3\";\npackage hooks;\nimport \"stego/common/common.proto\";\n// The hook.\nservice Hook { rpc Evaluate(common.Request) returns (common.Result); }\n",
			"common/common.proto":               "syntax = \"proto3\";\npackage common;\nimport \"stego/common/base.proto\";\nmessage Request { base.Id id = 1; }\nmessage Result { bool ok = 1; }\n",
			"common/base.proto":                 "syntax = \"proto3\";\npackage base;\n// The base.\nmessage Id { string value = 1; }\n",
			"common/extra.proto":                "syntax = \"proto3\";\npackage extra;\n// Nothing imports this file.\nmessage Extra { string value = 1; }\n",
		}
		root := writeFiles(t, files, sortedNames(files, false))
		contract := "components/hooks/slots/hook.proto"
		first := mustContractDigest(t, dirContracts(root), contract)
		restore := changeComment(t, filepath.Join(root, "common/base.proto"))
		second := mustContractDigest(t, dirContracts(root), contract)
		restore()
		restore = changeComment(t, filepath.Join(root, "common/extra.proto"))
		third := mustContractDigest(t, dirContracts(root), contract)
		restore()
		forward := mustContractDigest(t, dirContracts(writeFiles(t, files, sortedNames(files, false))), contract)
		backward := mustContractDigest(t, dirContracts(writeFiles(t, files, sortedNames(files, true))), contract)
		if second == first {
			t.Fatal("a change to a file imported through another import leaves the digest")
		}
		if third != first || forward != first || backward != first {
			t.Fatalf("the digest changes with a file that no file imports, or with the order of the files: %s %s %s %s", first, third, forward, backward)
		}
	})
}

// dirContracts reads slot contracts from a registry folder, and resolves an
// import with the optional stego/ prefix inside it, as the registry does.
type dirContracts string

func (d dirContracts) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(string(d), filepath.FromSlash(name)))
}

func (d dirContracts) ReadProtoImport(name string) ([]byte, error) {
	if data, err := d.ReadFile(strings.TrimPrefix(name, "stego/")); err == nil {
		return data, nil
	}
	return d.ReadFile(name)
}

func mustContractDigest(t *testing.T, files ContractFiles, contract string) string {
	t.Helper()
	digest, err := ContractDigest(files, contract)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// copyRegistry copies the built-in registry of the repository into a new
// folder.
func copyRegistry(t *testing.T) string {
	t.Helper()
	source, err := filepath.Abs("../../registry")
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	err = filepath.WalkDir(source, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(source, name)
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(target, relative), 0755)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(target, relative), data, 0644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

// changeComment adds one byte to a comment line of the file, or adds a
// comment line when it has none, and gives a function that restores it.
func changeComment(t *testing.T, path string) func() {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if index := strings.Index(text, "//"); index >= 0 {
		text = text[:index+2] + "x" + text[index+2:]
	} else {
		text += "// x\n"
	}
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, line...), 0644); err != nil {
		t.Fatal(err)
	}
}

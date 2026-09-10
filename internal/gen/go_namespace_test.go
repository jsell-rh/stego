package gen

import "testing"

func TestGoLibraryNamespaces(t *testing.T) {
	for _, name := range []string{"controller", "go-services/controller", "v2/worker_2", "libs/init", "libs/string", "libs/_record"} {
		if err := ValidateGoPackageNamespace(name); err != nil {
			t.Errorf("valid library %q: %v", name, err)
		}
	}
	for _, name := range []string{"", ".", "../worker", "a//worker", "a/../worker", "bad-name", "workers/type", "workers/_", "workers/main", "workers/9worker", "workers/café", "workers/bad name", "bad space/worker", "bad~1/worker"} {
		if err := ValidateGoPackageNamespace(name); err == nil {
			t.Errorf("invalid library %q was accepted", name)
		}
	}
}

func TestGoPackageContainersHaveNoDerivedPackageName(t *testing.T) {
	for _, name := range []string{"cli-tools", "nested/cli-tools", "main", "type", "9tools"} {
		if err := ValidateGoImportNamespace(name); err != nil {
			t.Errorf("valid package container %q: %v", name, err)
		}
	}
	for _, name := range []string{"bad space", "nested/café", "../cli", "a//cli"} {
		if err := ValidateGoImportNamespace(name); err == nil {
			t.Errorf("invalid package container %q was accepted", name)
		}
	}
}

// Package buildidentity reports selected metadata from the Go build record.
package buildidentity

import (
	"errors"
	"runtime"
	"runtime/debug"
)

// Record describes the build toolchain and source metadata. It is not an
// artifact digest or a signature. Unknown source data is never reported clean.
// Build paths, dependency lists, and linker arguments are not exposed.
type Record struct {
	GoVersion     string `json:"go_version" yaml:"go_version"`
	GOOS          string `json:"goos" yaml:"goos"`
	GOARCH        string `json:"goarch" yaml:"goarch"`
	ModuleVersion string `json:"module_version" yaml:"module_version"`
	VCS           string `json:"vcs" yaml:"vcs"`
	Revision      string `json:"revision" yaml:"revision"`
	SourceState   string `json:"source_state" yaml:"source_state"`
}

// Current reads the executable metadata. It does not inspect the working tree,
// read application configuration, start a process, or make a network request.
func Current() Record {
	info, _ := debug.ReadBuildInfo()
	return fromInfo(info)
}

func fromInfo(info *debug.BuildInfo) Record {
	r := Record{GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, ModuleVersion: "unknown", VCS: "unknown", Revision: "unknown", SourceState: "unknown"}
	if info == nil {
		return r
	}
	if safe(info.GoVersion) {
		r.GoVersion = info.GoVersion
	}
	if safe(info.Main.Version) {
		r.ModuleVersion = info.Main.Version
	}
	values := map[string]string{}
	duplicates := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs", "vcs.revision", "vcs.modified":
			if _, ok := values[s.Key]; ok {
				duplicates = true
			}
			values[s.Key] = s.Value
		}
	}
	if !duplicates && safe(values["vcs"]) && safe(values["vcs.revision"]) {
		r.VCS, r.Revision = values["vcs"], values["vcs.revision"]
		switch values["vcs.modified"] {
		case "true":
			r.SourceState = "modified"
		case "false":
			r.SourceState = "clean"
		}
	}
	return r
}

func safe(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, c := range value {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

// Validate rejects incomplete records and invalid source states in saved data.
func (r Record) Validate() error {
	for _, v := range []string{r.GoVersion, r.GOOS, r.GOARCH, r.ModuleVersion, r.VCS, r.Revision} {
		if !safe(v) {
			return errors.New("invalid build record")
		}
	}
	switch r.SourceState {
	case "unknown", "clean", "modified":
	default:
		return errors.New("invalid build source state")
	}
	if (r.VCS == "unknown" || r.Revision == "unknown") && (r.VCS != "unknown" || r.Revision != "unknown" || r.SourceState != "unknown") {
		return errors.New("incomplete build source record")
	}
	return nil
}

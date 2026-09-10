package buildidentity

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestSourceState(t *testing.T) {
	for _, tc := range []struct{ name, modified, want string }{{"clean", "false", "clean"}, {"changed", "true", "modified"}, {"absent", "", "unknown"}, {"invalid", "False", "unknown"}} {
		t.Run(tc.name, func(t *testing.T) {
			info := &debug.BuildInfo{GoVersion: "go1.26.8", Main: debug.Module{Version: "v1.2.3"}, Settings: []debug.BuildSetting{{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: strings.Repeat("a", 40)}, {Key: "vcs.modified", Value: tc.modified}, {Key: "-ldflags", Value: "private-build-argument"}}}
			got := fromInfo(info)
			if got.SourceState != tc.want || got.ModuleVersion != "v1.2.3" || got.Validate() != nil {
				t.Fatal(got)
			}
		})
	}
	for _, info := range []*debug.BuildInfo{nil, {}, {Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "false"}}}, {Settings: []debug.BuildSetting{{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: "a"}, {Key: "vcs", Value: "git"}, {Key: "vcs.modified", Value: "false"}}}} {
		got := fromInfo(info)
		if got.SourceState != "unknown" || got.Revision != "unknown" || got.Validate() != nil {
			t.Fatal(got)
		}
	}
}

func TestInvalidRecord(t *testing.T) {
	good := Current()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*Record){func(r *Record) { r.GoVersion = "" }, func(r *Record) { r.Revision = "bad\nrevision" }, func(r *Record) { r.GOOS = strings.Repeat("x", 257) }, func(r *Record) { r.SourceState = "trusted" }, func(r *Record) { r.VCS = "unknown"; r.Revision = "a" }, func(r *Record) { r.VCS = "unknown"; r.Revision = "unknown"; r.SourceState = "clean" }} {
		r := good
		edit(&r)
		if r.Validate() == nil {
			t.Fatal("invalid record accepted", r)
		}
	}
}

func BenchmarkCurrent(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = Current()
	}
}

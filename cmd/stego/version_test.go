package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jsell-rh/stego/internal/buildidentity"
)

type failedVersionWriter struct{}

func (failedVersionWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
func TestVersion(t *testing.T) {
	var out bytes.Buffer
	if err := runVersion(nil, &out); err != nil || out.String() != "stego "+version+"\n" {
		t.Fatal(out.String(), err)
	}
	out.Reset()
	if err := runVersion([]string{"--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Version string               `json:"version"`
		Build   buildidentity.Record `json:"build"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Build != buildidentity.Current() || report.Version != version {
		t.Fatal(report, err)
	}
	for _, args := range [][]string{{"--json", "extra"}, {"invalid"}} {
		out.Reset()
		if err := runVersion(args, &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid arguments accepted")
		}
	}
	if err := runVersion([]string{"--json"}, failedVersionWriter{}); err == nil {
		t.Fatal("write failure ignored")
	}
}

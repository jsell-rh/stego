package browserassets

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func fixture() []Asset {
	return []Asset{{"index.html", []byte(`<!doctype html><html><script src="/assets/app.js"></script></html>`)}, {"assets/app.js", []byte(`"use strict";`)}}
}
func TestBundleRoundTrip(t *testing.T) {
	first, err := Encode(fixture())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Encode(fixture())
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("bundle changed", err)
	}
	decoded, err := Decode(first)
	if err != nil {
		t.Fatal(err)
	}
	if decoded[0].Path != "assets/app.js" || decoded[1].Path != "index.html" {
		t.Fatal("incorrect asset order")
	}
	if !bytes.Equal(decoded[0].Data, fixture()[1].Data) {
		t.Fatal("asset changed")
	}
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, asset := range fixture() {
		if err := os.WriteFile(filepath.Join(directory, filepath.FromSlash(asset.Path)), asset.Data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	packed, err := PackDirectory(directory)
	if err != nil || !bytes.Equal(first, packed) {
		t.Fatal("directory differs", err)
	}
	target := filepath.Join(t.TempDir(), "assets.zip")
	if err := WriteFile(target, packed); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(target, []byte("invalid")); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	saved, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(saved, first) {
		t.Fatal("invalid replacement changed output", err)
	}
	link := filepath.Join(directory, "assets", "link.js")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := PackDirectory(directory); err == nil {
		t.Fatal("symbolic asset accepted")
	}
}
func TestBundleRejectsInvalidEntries(t *testing.T) {
	for _, name := range []string{"../index.html", "/index.html", "assets/../app.js", "assets/.secret.js", "assets/app.js.map", "assets/app.js?x", "assets\\app.js", "assets/dir//app.js"} {
		assets := append(fixture(), Asset{name, []byte("x")})
		if _, err := Encode(assets); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	for _, assets := range [][]Asset{fixture()[1:], append(fixture(), fixture()[0]), {{"index.html", nil}}, {{"index.html", bytes.Repeat([]byte("x"), MaxFile+1)}}} {

		if _, err := Encode(assets); err == nil {
			t.Fatal("invalid asset set accepted")
		}
	}
	var duplicate bytes.Buffer
	writer := zip.NewWriter(&duplicate)
	for i := 0; i < 2; i++ {
		file, _ := writer.Create("index.html")
		file.Write([]byte("x"))
	}
	writer.Close()
	if _, err := Decode(duplicate.Bytes()); err == nil {
		t.Fatal("duplicate ZIP entry accepted")
	}
	var bomb bytes.Buffer
	writer = zip.NewWriter(&bomb)
	file, _ := writer.Create("index.html")
	file.Write(bytes.Repeat([]byte("x"), MaxFile+1))
	writer.Close()
	if _, err := Decode(bomb.Bytes()); err == nil {
		t.Fatal("expanded ZIP limit ignored")
	}
}
func TestCapturedScriptPolicy(t *testing.T) {
	code := "window.ready = true;\n"
	input := []byte("<html><script src=\"/assets/app.js\"></script><script>" + code + "</script></html>")
	sum := sha256.Sum256([]byte(code))
	expected := []string{"'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"}
	hashes, err := ScriptHashes(input, map[string]bool{"assets/app.js": true})
	if err != nil || !reflect.DeepEqual(hashes, expected) {
		t.Fatal("script policy differs", hashes, err)
	}
	// HTML keeps the first attribute when a name occurs twice.
	if _, err := ScriptHashes([]byte(`<script src="/assets/app.js" src="/other.js"></script>`), map[string]bool{"assets/app.js": true}); err != nil {
		t.Fatal("first HTML attribute was not retained", err)
	}
	for _, bad := range []string{`<script src="https://example.test/app.js"></script>`, `<script src="/assets/missing.js"></script>`, `<script src="/assets/app.js">alert(1)</script>`, `<script src="/other.js" src="/assets/app.js"></script>`, `<img onerror="alert(1)">`, `<style>body{color:red}</style>`, `<div style="color:red">`, `<script nonce="fixed">x</script>`, `<script>unfinished`, `<base href="https://example.test">`, `<link rel="stylesheet" href="https://example.test/app.css">`} {
		if _, err := ScriptHashes([]byte(bad), map[string]bool{"assets/app.js": true}); err == nil {
			t.Fatalf("invalid HTML accepted: %s", bad)
		}
	}
}

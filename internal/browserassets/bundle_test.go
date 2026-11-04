package browserassets_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jsell-rh/stego/internal/browserassets"
	"github.com/jsell-rh/stego/internal/compiler"
)

func TestBundleAboveOneMiBRetainsBounds(t *testing.T) {
	payload := make([]byte, (1<<20)+(256<<10))
	_, _ = rand.New(rand.NewSource(1)).Read(payload)
	assets := append(fixture(), browserassets.Asset{Path: "assets/payload.png", Data: payload})
	encoded, err := browserassets.Encode(assets)
	if err != nil || len(encoded) <= 1<<20 {
		t.Fatal("larger bounded bundle was not encoded", len(encoded), err)
	}
	decoded, err := browserassets.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	again, err := browserassets.Encode(decoded)
	if err != nil || !bytes.Equal(again, encoded) {
		t.Fatal("larger bundle changed during round trip", err)
	}
	if _, err := browserassets.Decode(make([]byte, browserassets.MaxBundle+1)); err == nil {
		t.Fatal("oversized captured bundle was accepted")
	}
}

func fixture() []browserassets.Asset {
	return []browserassets.Asset{{Path: "index.html", Data: []byte(`<!doctype html><html><script src="/assets/app.js"></script></html>`)}, {Path: "assets/app.js", Data: []byte(`"use strict";`)}}
}
func TestBundleRoundTrip(t *testing.T) {
	first, err := browserassets.Encode(fixture())
	if err != nil {
		t.Fatal(err)
	}
	second, err := browserassets.Encode(fixture())
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("bundle changed", err)
	}
	decoded, err := browserassets.Decode(first)
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
	packed, err := compiler.PackBrowserAssets(directory)
	if err != nil || !bytes.Equal(first, packed) {
		t.Fatal("directory differs", err)
	}
	target := filepath.Join(t.TempDir(), "assets.zip")
	if err := compiler.WriteBrowserAssets(target, packed); err != nil {
		t.Fatal(err)
	}
	if err := compiler.WriteBrowserAssets(target, []byte("invalid")); err == nil {
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
	if _, err := compiler.PackBrowserAssets(directory); err == nil {
		t.Fatal("symbolic asset accepted")
	}
}
func TestTrueTypeBundle(t *testing.T) {
	assets := append(fixture(), browserassets.Asset{Path: "assets/symbols.ttf", Data: []byte{0, 1, 0, 0, 255, 128}})
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, asset := range assets {
		if err := os.WriteFile(filepath.Join(directory, filepath.FromSlash(asset.Path)), asset.Data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	packed, err := compiler.PackBrowserAssets(directory)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := browserassets.Decode(packed)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 3 || decoded[1].Path != assets[2].Path || !bytes.Equal(decoded[1].Data, assets[2].Data) {
		t.Fatal("font bytes changed")
	}
	again, err := browserassets.Encode(decoded)
	if err != nil || !bytes.Equal(packed, again) {
		t.Fatal("font bundle changed", err)
	}
	for _, name := range []string{"assets/../symbols.ttf", "assets/.symbols.ttf", "assets/symbols.ttf.js.map", "symbols.ttf"} {
		if _, err := browserassets.Encode(append(fixture(), browserassets.Asset{name, assets[2].Data})); err == nil {
			t.Fatalf("invalid font path accepted: %s", name)
		}
	}
}

func TestBundleRejectsInvalidEntries(t *testing.T) {
	for _, name := range []string{"../index.html", "/index.html", "assets/../app.js", "assets/.secret.js", "assets/app.js.map", "assets/app.js?x", "assets\\app.js", "assets/dir//app.js"} {
		assets := append(fixture(), browserassets.Asset{name, []byte("x")})
		if _, err := browserassets.Encode(assets); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	for _, assets := range [][]browserassets.Asset{fixture()[1:], append(fixture(), fixture()[0]), {{"index.html", nil}}, {{"index.html", bytes.Repeat([]byte("x"), browserassets.MaxFile+1)}}} {

		if _, err := browserassets.Encode(assets); err == nil {
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
	if _, err := browserassets.Decode(duplicate.Bytes()); err == nil {
		t.Fatal("duplicate ZIP entry accepted")
	}
	var bomb bytes.Buffer
	writer = zip.NewWriter(&bomb)
	file, _ := writer.Create("index.html")
	file.Write(bytes.Repeat([]byte("x"), browserassets.MaxFile+1))
	writer.Close()
	if _, err := browserassets.Decode(bomb.Bytes()); err == nil {
		t.Fatal("expanded ZIP limit ignored")
	}
}
func TestCapturedScriptPolicy(t *testing.T) {
	code := "window.ready = true;\n"
	input := []byte("<html><script src=\"/assets/app.js\"></script><script>" + code + "</script></html>")
	sum := sha256.Sum256([]byte(code))
	expected := []string{"'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"}
	hashes, err := browserassets.ScriptHashes(input, map[string]bool{"assets/app.js": true})
	if err != nil || !reflect.DeepEqual(hashes, expected) {
		t.Fatal("script policy differs", hashes, err)
	}
	// HTML keeps the first attribute when a name occurs twice.
	if _, err := browserassets.ScriptHashes([]byte(`<script src="/assets/app.js" src="/other.js"></script>`), map[string]bool{"assets/app.js": true}); err != nil {
		t.Fatal("first HTML attribute was not retained", err)
	}
	for _, bad := range []string{`<script src="https://example.test/app.js"></script>`, `<script src="/assets/missing.js"></script>`, `<script src="/assets/app.js">alert(1)</script>`, `<script src="/other.js" src="/assets/app.js"></script>`, `<img onerror="alert(1)">`, `<style>body{color:red}</style>`, `<div style="color:red">`, `<script nonce="fixed">x</script>`, `<script>unfinished`, `<base href="https://example.test">`, `<link rel="stylesheet" href="https://example.test/app.css">`} {
		if _, err := browserassets.ScriptHashes([]byte(bad), map[string]bool{"assets/app.js": true}); err == nil {
			t.Fatalf("invalid HTML accepted: %s", bad)
		}
	}
}

// Package browserassets captures bounded, reproducible browser asset bundles.
package browserassets

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const MaxBundle = 1 << 20
const MaxFile = 4 << 20
const MaxTotal = 16 << 20
const MaxFiles = 128

type Asset struct {
	Path string
	Data []byte
}

var namePattern = regexp.MustCompile(`^(index\.html|assets/[A-Za-z0-9_./-]+)$`)

func ValidName(name string) bool {
	if len(name) > 256 || !namePattern.MatchString(name) || path.Clean(name) != name || strings.Contains(name, "//") {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if strings.HasPrefix(segment, ".") {
			return false
		}
	}
	switch path.Ext(name) {
	case ".html":
		return name == "index.html"
	case ".js", ".css", ".svg", ".png", ".ico", ".woff2":
		return true
	}
	return false
}
func validate(assets []Asset) error {
	if len(assets) == 0 || len(assets) > MaxFiles {
		return fmt.Errorf("browser bundle requires 1 through %d assets", MaxFiles)
	}
	total := 0
	previous := ""
	index := false
	for _, asset := range assets {
		if !ValidName(asset.Path) || asset.Path <= previous || len(asset.Data) == 0 || len(asset.Data) > MaxFile {
			return fmt.Errorf("invalid, duplicate, unordered, or oversized browser asset")
		}
		previous = asset.Path
		total += len(asset.Data)
		index = index || asset.Path == "index.html"
		if total > MaxTotal {
			return fmt.Errorf("browser bundle exceeds its expanded size limit")
		}
	}
	if !index {
		return fmt.Errorf("browser bundle requires index.html")
	}
	return nil
}
func Encode(assets []Asset) ([]byte, error) {
	assets = append([]Asset(nil), assets...)
	sort.Slice(assets, func(i, j int) bool { return assets[i].Path < assets[j].Path })
	if err := validate(assets); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, asset := range assets {
		header := &zip.FileHeader{Name: asset.Path, Method: zip.Deflate}
		header.SetMode(0644)
		file, err := writer.CreateHeader(header)
		if err != nil {
			return nil, err
		}
		if _, err = file.Write(asset.Data); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if output.Len() > MaxBundle {
		return nil, fmt.Errorf("browser bundle exceeds its captured size limit")
	}
	return output.Bytes(), nil
}
func Decode(data []byte) ([]Asset, error) {
	if len(data) == 0 || len(data) > MaxBundle {
		return nil, fmt.Errorf("invalid browser bundle size")
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid browser bundle")
	}
	if len(reader.File) == 0 || len(reader.File) > MaxFiles {
		return nil, fmt.Errorf("invalid browser bundle file count")
	}
	assets := make([]Asset, 0, len(reader.File))
	total := 0
	previous := ""
	for _, entry := range reader.File {
		if !ValidName(entry.Name) || entry.Name <= previous || !entry.Mode().IsRegular() || entry.UncompressedSize64 > MaxFile || (entry.Method != zip.Store && entry.Method != zip.Deflate) {
			return nil, fmt.Errorf("invalid browser bundle entry")
		}
		previous = entry.Name
		file, err := entry.Open()
		if err != nil {
			return nil, fmt.Errorf("invalid browser bundle content")
		}
		content, readErr := io.ReadAll(io.LimitReader(file, MaxFile+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(content) > MaxFile {
			return nil, fmt.Errorf("invalid browser bundle content")
		}
		total += len(content)
		if total > MaxTotal {
			return nil, fmt.Errorf("browser bundle exceeds its expanded size limit")
		}
		assets = append(assets, Asset{entry.Name, content})
	}
	if err := validate(assets); err != nil {
		return nil, err
	}
	return assets, nil
}
func PackDirectory(directory string) ([]byte, error) {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("browser asset source must be a directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var assets []Asset
	nodes, total := 0, 0
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		nodes++
		if nodes > 512 {
			return fmt.Errorf("browser asset tree exceeds its limit")
		}
		if entry.IsDir() {
			if name != "." && name != "assets" && !strings.HasPrefix(name, "assets/") {
				return fmt.Errorf("unexpected browser asset directory")
			}
			return nil
		}
		if !entry.Type().IsRegular() || !ValidName(name) || len(assets) >= MaxFiles {
			return fmt.Errorf("invalid browser asset file")
		}
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > MaxFile {
			file.Close()
			return fmt.Errorf("invalid browser asset file")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, MaxFile+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(data) > MaxFile {
			return fmt.Errorf("cannot read browser asset")
		}
		total += len(data)
		if total > MaxTotal {
			return fmt.Errorf("browser assets exceed their total size limit")
		}
		assets = append(assets, Asset{name, data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return Encode(assets)
}

// WriteFile replaces one bundle after all input checks pass.
func WriteFile(name string, data []byte) error {
	if _, err := Decode(data); err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer root.Close()
	base := filepath.Base(name)
	if info, err := root.Lstat(base); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("bundle output must be a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temp := ".stego-assets-" + hex.EncodeToString(random[:])
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = root.Rename(temp, base); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

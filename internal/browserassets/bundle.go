// Package browserassets is the format of bounded, reproducible browser asset
// bundles, which the compiler captures and the browser backend generator reads.
package browserassets

import (
	"archive/zip"
	"bytes"

	"fmt"
	"io"

	"path"

	"regexp"
	"sort"
	"strings"
)

const MaxBundle = 4 << 20
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
	case ".js", ".css", ".svg", ".png", ".ico", ".woff2", ".ttf":
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

package compiler

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jsell-rh/stego/internal/browserassets"
)

// PackBrowserAssets is the handler of ST-GEN-CMD04, Capture a browser asset
// bundle: it checks every entry of the frontend build folder and encodes the
// bundle in the format of internal/browserassets.
func PackBrowserAssets(directory string) ([]byte, error) {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("browser asset source must be a directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var assets []browserassets.Asset
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
		if !entry.Type().IsRegular() || !browserassets.ValidName(name) || len(assets) >= browserassets.MaxFiles {
			return fmt.Errorf("invalid browser asset file")
		}
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > browserassets.MaxFile {
			file.Close()
			return fmt.Errorf("invalid browser asset file")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, browserassets.MaxFile+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(data) > browserassets.MaxFile {
			return fmt.Errorf("cannot read browser asset")
		}
		total += len(data)
		if total > browserassets.MaxTotal {
			return fmt.Errorf("browser assets exceed their total size limit")
		}
		assets = append(assets, browserassets.Asset{Path: name, Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return browserassets.Encode(assets)
}

// WriteBrowserAssets replaces one bundle after all input checks pass.
func WriteBrowserAssets(name string, data []byte) error {
	if _, err := browserassets.Decode(data); err != nil {
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

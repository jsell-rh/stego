package appbuild

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/dirhash"
	modzip "golang.org/x/mod/zip"
)

// ModuleCacheRecord describes the download cache produced by Download. The
// cache holds only the download cache (zips) that the Go toolchain extracts
// again during each offline build. It never holds extracted module sources,
// because the toolchain trusts those without checking go.sum.
type ModuleCacheRecord struct {
	Format          int            `json:"format"`
	SourceRevision  string         `json:"source_revision"`
	Module          string         `json:"module"`
	Target          string         `json:"target"`
	GoVersion       string         `json:"go_version"`
	GitSHA256       string         `json:"git_sha256"`
	DownloadCache   Inventory      `json:"download_cache"`
	Modules         []CachedModule `json:"modules"`
	DependencyProxy string         `json:"dependency_proxy"`
}

// CachedModule is one module zip in a module cache: its path, its version,
// the hash that go.sum states for it, which its bytes match, and the source
// that supplied it (ST-BLD-UC02-R05).
type CachedModule struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Sum     string `json:"sum"`
	Source  string `json:"source"`
}

// moduleCacheFormat is the format of module-cache.json. Format 2 lists each
// module zip by path, version and hash; format 3 adds the source of each.
const moduleCacheFormat = 3

// ProxySource names the public module proxy as the source of a module.
const ProxySource = "https://proxy.golang.org"

// RuntimeFolder is the common registry's runtime folder in the source
// revision: the folder of the runtime library's module path in STEGO's own
// repository.
const RuntimeFolder = "registry/runtime"

// RuntimeModule is the module path of the runtime library, the path that
// Supplying gives it (RuntimeModule). Building may not import
// Supplying (ST-STD-CODE-R03), so the path is stated here again; the system
// tests of ST-BLD-UC02-R05 build their fixture from RuntimeModule,
// so a difference fails them.
const RuntimeModule = "github.com/jsell-rh/stego/registry/runtime"

// runtimePins are the runtime pins of STEGO's own registry, by version: the
// go.sum hash of the module zip that each archetype in registry/archetypes
// pins (ST-SUP-INV10). The binary carries them, so a source revision cannot
// add one. The go.mod hash needs no entry: the go.mod is in the zip, so the
// zip hash fixes it. A test holds this table equal to the archetypes'
// pins; a moved pin changes it in the same change (ST-BLD-UC02-R05).
var runtimePins = map[string]string{
	"v0.43.0": "h1:/qXNaoTWnQuyeqDpE1iM2fMGc/B76+AbShg1PCWyvlc=",
}

// RuntimePins gives a copy of the runtime pins that this binary trusts, by
// version.
func RuntimePins() map[string]string {
	pins := make(map[string]string, len(runtimePins))
	for version, sum := range runtimePins {
		pins[version] = sum
	}
	return pins
}

// runtimeTrust is what a runtime folder must also match: the pins the binary
// carries, and the hash that the sum database knows, if it knows one.
type runtimeTrust struct {
	pins   map[string]string
	lookup func(ctx context.Context, path, version string) (sum string, found bool, err error)
}

// defaultRuntimeTrust is the trust of a real download: the built-in pins and
// sum.golang.org.
func defaultRuntimeTrust() runtimeTrust {
	return runtimeTrust{pins: runtimePins, lookup: lookupSumDB}
}

// errSumDBInconsistent is a sum database answer that its signed tree does not
// prove. It refuses the runtime folder; any other lookup error does not.
var errSumDBInconsistent = errors.New("the sum database is not consistent")

// sumDBKey is the public key of sum.golang.org, the key the go command uses.
const sumDBKey = "sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8"

// lookupSumDB asks sum.golang.org for the go.sum hash of module@version and
// checks the answer against the signed tree of the database. It gives
// found false when the database does not know the module, as for a version
// that is not released.
func lookupSumDB(ctx context.Context, path, version string) (string, bool, error) {
	return lookupSumDBAt(ctx, sumDBServer{url: "https://sum.golang.org", key: sumDBKey, client: http.DefaultClient}, path, version)
}

// sumDBServer is the sum database that a lookup asks: its URL, its public
// key and the HTTP client that reaches it. Tests give a local stand-in.
type sumDBServer struct {
	url, key string
	client   *http.Client
}

// lookupSumDBAt is lookupSumDB against the sum database server.
func lookupSumDBAt(ctx context.Context, server sumDBServer, path, version string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ops := &sumDBOps{ctx: ctx, server: server, config: map[string][]byte{"key": []byte(server.key)}, cache: map[string][]byte{}}
	lines, err := sumdb.NewClient(ops).Lookup(path, version)
	if ops.security != "" {
		return "", false, fmt.Errorf("%w: %s", errSumDBInconsistent, ops.security)
	}
	if ops.notFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == path && fields[1] == version {
			return fields[2], true, nil
		}
	}
	return "", false, fmt.Errorf("the sum database gives no hash for %s %s", path, version)
}

// sumDBOps is the client side of one sum database lookup: it keeps the
// signed tree and the tiles in memory and reads the rest over HTTPS.
type sumDBOps struct {
	ctx      context.Context
	server   sumDBServer
	config   map[string][]byte
	cache    map[string][]byte
	notFound bool
	security string
}

func (o *sumDBOps) ReadRemote(path string) ([]byte, error) {
	request, err := http.NewRequestWithContext(o.ctx, http.MethodGet, o.server.url+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := o.server.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(path, "/lookup/") && (response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone) {
		o.notFound = true
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sum database %s: %s", path, response.Status)
	}
	return data, nil
}

func (o *sumDBOps) ReadConfig(file string) ([]byte, error) {
	return o.config[file], nil
}

func (o *sumDBOps) WriteConfig(file string, old, new []byte) error {
	if !bytes.Equal(o.config[file], old) {
		return sumdb.ErrWriteConflict
	}
	o.config[file] = new
	return nil
}

func (o *sumDBOps) ReadCache(file string) ([]byte, error) {
	if data, ok := o.cache[file]; ok {
		return data, nil
	}
	return nil, os.ErrNotExist
}

func (o *sumDBOps) WriteCache(file string, data []byte) { o.cache[file] = data }

func (o *sumDBOps) Log(string) {}

func (o *sumDBOps) SecurityError(message string) { o.security = message }

// maxGoModBytes limits a go.mod file that STEGO reads.
const maxGoModBytes = 1 << 20

// RuntimeFolderSource names the common registry's runtime folder of the
// source revision as the source of a module. Only the runtime pin can have
// it (ST-BLD-UC02-R05).
const RuntimeFolderSource = "source:" + RuntimeFolder

// DownloadOptions selects the source revision and the new cache directory.
type DownloadOptions struct {
	Source   string
	Revision string
	Module   string
	Target   string
	Go       string
	Output   string
}

// Download fills a new directory with the module download cache for one
// source revision and writes a record that binds the cache contents to that
// revision. The cache directory is suitable for offline builds. The network
// phase uses the same fixed environment as a normal build.
func Download(ctx context.Context, options DownloadOptions) (*ModuleCacheRecord, error) {
	if ctx == nil {
		return nil, errors.New("module cache download requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	o, err := prepareDownload(options)
	if err != nil {
		return nil, err
	}
	sdk := filepath.Dir(filepath.Dir(o.Go))
	if _, err := checkSDK(sdk); err != nil {
		return nil, err
	}
	git := "/usr/bin/git"
	gitHash, _, err := fileDigest(git)
	if err != nil {
		return nil, err
	}
	// Use a private tree for the network phase so the recorded cache only
	// holds the download cache, never any toolchain state. The result is
	// moved to the output folder only after every check passes, and the
	// private tree is removed on every path, so a failed or interrupted
	// download leaves no cache folder and no record.
	root, err := os.MkdirTemp(filepath.Dir(o.Output), ".download-")
	if err != nil {
		return nil, err
	}
	defer removeTree(root)
	for _, name := range []string{"source", "home", "tmp", "modules", "cache", "gopath", "single"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			return nil, err
		}
	}
	env := environment(root, sdk, false)
	archive, err := os.OpenFile(filepath.Join(root, "source.tar"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	err = command(ctx, o.Source, env, archive, maxBytes+(32<<20), git, "archive", "--format=tar", o.Revision)
	if err == nil {
		_, err = archive.Seek(0, io.SeekStart)
	}
	if err == nil {
		err = unpack(archive, filepath.Join(root, "source"))
	}
	closeErr := archive.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	snapshot := filepath.Join(root, "source")
	module := filepath.Join(snapshot, filepath.FromSlash(o.Module))
	if err := checkModules(snapshot, module, map[string]bool{}); err != nil {
		return nil, err
	}
	sums, modSums, err := readSumFile(filepath.Join(module, "go.sum"))
	if err != nil {
		return nil, err
	}
	// The runtime pin comes from the common registry's runtime folder of the
	// source revision when that folder hashes to the pin (ST-BLD-UC02-R05).
	// It is served as a file proxy before the public proxy, which still
	// supplies every other module.
	pin, err := runtimePin(filepath.Join(module, "go.mod"))
	if err != nil {
		return nil, err
	}
	served, err := stageRuntimeFolder(ctx, filepath.Join(snapshot, filepath.FromSlash(RuntimeFolder)), pin, sums, modSums, filepath.Join(root, "runtime-proxy"), filepath.Join(root, "tmp"), defaultRuntimeTrust())
	if err != nil {
		return nil, err
	}
	if served != nil {
		env, err = withRuntimeProxy(env, filepath.Join(root, "runtime-proxy"))
		if err != nil {
			return nil, err
		}
	}
	// Download every module with the fixed environment. The go command checks
	// every zip against go.sum, and asks the sum database only about a module
	// that go.sum does not list.
	if _, err = capture(ctx, module, env, o.Go, "mod", "download"); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		single := environment(filepath.Join(root, "single"), sdk, false)
		return nil, diagnoseDownload(ctx, filepath.Join(root, "single"), single, o.Go, sums, served)
	}
	cacheRoot := filepath.Join(root, "modules")
	modules, err := checkCachedModules(filepath.Join(cacheRoot, "cache", "download"), sums)
	if err != nil {
		return nil, err
	}
	for index := range modules {
		modules[index].Source = ProxySource
		if served != nil && modules[index].Path == served.path && modules[index].Version == served.version {
			modules[index].Source = RuntimeFolderSource
		}
	}
	download, _, err := inventory(filepath.Join(cacheRoot, "cache", "download"), maxFiles, maxBytes)
	if err != nil {
		return nil, err
	}
	if download.Files == 0 {
		return nil, errors.New("module download produced no cache")
	}
	record := &ModuleCacheRecord{Format: moduleCacheFormat, SourceRevision: o.Revision, Module: o.Module, Target: o.Target, GoVersion: GoVersion, GitSHA256: gitHash, DownloadCache: download, Modules: modules, DependencyProxy: ProxySource}
	if err := validateModuleCacheRecord(record); err != nil {
		return nil, err
	}
	result := filepath.Join(root, "result")
	if err := copyTree(filepath.Join(cacheRoot, "cache", "download"), filepath.Join(result, "cache", "download")); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(result, "module-cache.json"), data, 0600); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := publishCache(result, o.Output); err != nil {
		return nil, err
	}
	return record, nil
}

// publishCache moves a complete cache into the new output folder. It creates
// the folder exclusively, moves the cache in, and writes the record last, so
// a folder with a record always holds its whole cache. A failure removes the
// folder it created.
func publishCache(result, output string) (err error) {
	if err := os.Mkdir(output, 0700); err != nil {
		return fmt.Errorf("module cache target %s: %w", output, err)
	}
	defer func() {
		if err != nil {
			_ = removeTree(output)
		}
	}()
	if err := os.Rename(filepath.Join(result, "cache"), filepath.Join(output, "cache")); err != nil {
		return err
	}
	return os.Rename(filepath.Join(result, "module-cache.json"), filepath.Join(output, "module-cache.json"))
}

// removeTree removes a folder that can hold the read-only module sources Go
// extracts. It makes each folder writable first.
func removeTree(root string) error {
	_ = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() && entry.Type()&fs.ModeSymlink == 0 {
			_ = os.Chmod(name, 0700)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// moduleVersion is one module of go.sum: its path and its version.
type moduleVersion struct{ path, version string }

// readSums reads the hashes of the module zips that go.sum states. Lines for
// go.mod files are not zips and are skipped.
func readSums(name string) (map[moduleVersion]string, error) {
	sums, _, err := readSumFile(name)
	return sums, err
}

// readSumFile reads go.sum: the hashes of the module zips, and apart from
// them the hashes of the go.mod files.
func readSumFile(name string) (map[moduleVersion]string, map[moduleVersion]string, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read the module requirements' checksums %s", filepath.Base(name))
	}
	sums := map[moduleVersion]string{}
	modSums := map[moduleVersion]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 || !validModuleSum(fields[2]) {
			return nil, nil, errors.New("go.sum holds a malformed line")
		}
		target := sums
		version := fields[1]
		if strings.HasSuffix(version, "/go.mod") {
			target = modSums
			version = strings.TrimSuffix(version, "/go.mod")
		}
		key := moduleVersion{fields[0], version}
		if previous, ok := target[key]; ok && previous != fields[2] {
			return nil, nil, fmt.Errorf("go.sum states two hashes for %s %s", key.path, fields[1])
		}
		target[key] = fields[2]
	}
	return sums, modSums, scanner.Err()
}

// runtimePin gives the version of the runtime library that the module's
// go.mod requires, or nil when it requires none.
func runtimePin(name string) (*moduleVersion, error) {
	data, err := os.ReadFile(name)
	if err != nil || len(data) > maxGoModBytes {
		return nil, fmt.Errorf("cannot read the module requirements %s", filepath.Base(name))
	}
	file, err := modfile.ParseLax(name, data, nil)
	if err != nil {
		return nil, fmt.Errorf("the module requirements %s do not parse: %w", filepath.Base(name), err)
	}
	for _, require := range file.Require {
		if require.Mod.Path == RuntimeModule {
			return &moduleVersion{require.Mod.Path, require.Mod.Version}, nil
		}
	}
	return nil, nil
}

// stageRuntimeFolder enforces ST-BLD-UC02-R05. When go.mod requires the
// runtime pin and the source revision holds the common registry's runtime
// folder, it writes the module zip of that folder as the pin, in the module
// proxy layout under proxy, and gives the pin. It first refuses a folder that
// holds a link or a special file. It hashes the zip it wrote, and the go.mod
// inside that zip, and refuses either when it differs from go.sum, before any
// byte of the zip goes into the proxy; the refusal names the module, its
// version, the hash go.sum states and the hash it found. It gives nil when
// go.mod requires no runtime library or the source holds no runtime folder;
// the public proxy then supplies every module.
func stageRuntimeFolder(ctx context.Context, folder string, pin *moduleVersion, sums, modSums map[moduleVersion]string, proxy, scratch string, trust runtimeTrust) (*moduleVersion, error) {
	if pin == nil {
		return nil, nil
	}
	info, err := os.Lstat(folder)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("the runtime folder %s is a link or a file; expected a folder", RuntimeFolder)
	}
	if err := checkPlainTree(folder); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(folder, "go.mod")); err != nil {
		return nil, nil
	}
	stated, ok := sums[*pin]
	if !ok {
		return nil, fmt.Errorf("module %s %s: go.sum states no hash for it, so the runtime folder %s cannot be checked", pin.path, pin.version, RuntimeFolder)
	}
	statedMod, ok := modSums[*pin]
	if !ok {
		return nil, fmt.Errorf("module %s %s: go.sum states no hash for its go.mod, so the runtime folder %s cannot be checked", pin.path, pin.version, RuntimeFolder)
	}
	work, err := os.MkdirTemp(scratch, "runtime-")
	if err != nil {
		return nil, err
	}
	defer removeTree(work)
	zipPath := filepath.Join(work, "module.zip")
	file, err := os.OpenFile(zipPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	err = modzip.CreateFromDir(file, module.Version{Path: pin.path, Version: pin.version}, folder)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("module %s %s: the runtime folder %s gives no module zip: %w", pin.path, pin.version, RuntimeFolder, err)
	}
	found, err := dirhash.HashZip(zipPath, dirhash.Hash1)
	if err != nil {
		return nil, err
	}
	if found != stated {
		return nil, fmt.Errorf("module %s %s differs from go.sum: go.sum states %s, the runtime folder %s is %s", pin.path, pin.version, stated, RuntimeFolder, found)
	}
	// go.sum is part of the source revision, so it alone cannot make the
	// folder the pin: the folder must also be a pin of STEGO's own registry
	// that this binary carries.
	pinned, ok := trust.pins[pin.version]
	if !ok {
		return nil, fmt.Errorf("module %s %s: the runtime folder %s is %s, but STEGO's registry pins no runtime library at %s; expected one of the pins %s", pin.path, pin.version, RuntimeFolder, found, pin.version, pinList(trust.pins))
	}
	if found != pinned {
		return nil, fmt.Errorf("module %s %s differs from STEGO's registry pin: the pin is %s, the runtime folder %s is %s", pin.path, pin.version, pinned, RuntimeFolder, found)
	}
	// The sum database may know a released version. A hash it knows must be
	// the same. A version it does not know, or a database that cannot be
	// reached, leaves the registry pin as the check.
	known, foundKnown, lookupErr := trust.lookup(ctx, pin.path, pin.version)
	if errors.Is(lookupErr, errSumDBInconsistent) {
		return nil, fmt.Errorf("module %s %s: %w", pin.path, pin.version, lookupErr)
	}
	if lookupErr == nil && foundKnown && known != found {
		return nil, fmt.Errorf("module %s %s differs from the sum database: sum.golang.org states %s, the runtime folder %s is %s", pin.path, pin.version, known, RuntimeFolder, found)
	}
	modData, err := zipGoMod(zipPath, *pin)
	if err != nil {
		return nil, err
	}
	foundMod, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(modData)), nil
	})
	if err != nil {
		return nil, err
	}
	if foundMod != statedMod {
		return nil, fmt.Errorf("module %s %s go.mod differs from go.sum: go.sum states %s, the runtime folder %s is %s", pin.path, pin.version, statedMod, RuntimeFolder, foundMod)
	}
	escapedPath, err := module.EscapePath(pin.path)
	if err != nil {
		return nil, err
	}
	escapedVersion, err := module.EscapeVersion(pin.version)
	if err != nil {
		return nil, err
	}
	versions := filepath.Join(proxy, filepath.FromSlash(escapedPath), "@v")
	if err := os.MkdirAll(versions, 0o700); err != nil {
		return nil, err
	}
	infoData := fmt.Sprintf("{\"Version\":%q}\n", pin.version)
	for name, data := range map[string][]byte{escapedVersion + ".mod": modData, escapedVersion + ".info": []byte(infoData)} {
		if err := os.WriteFile(filepath.Join(versions, name), data, 0o600); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(zipPath, filepath.Join(versions, escapedVersion+".zip")); err != nil {
		return nil, err
	}
	return pin, nil
}

// pinList names the pins, in version order, for an error.
func pinList(pins map[string]string) string {
	names := make([]string, 0, len(pins))
	for version, sum := range pins {
		names = append(names, version+" "+sum)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// checkPlainTree refuses a folder that holds a link or a special file, at any
// depth, and names it. The walk does not follow links, so it reads nothing
// outside the folder.
func checkPlainTree(folder string) error {
	return filepath.WalkDir(folder, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Type().IsRegular() {
			return nil
		}
		relative, relErr := filepath.Rel(folder, name)
		if relErr != nil {
			relative = name
		}
		return fmt.Errorf("the runtime folder %s holds %s, a link or a special file; expected only files and folders", RuntimeFolder, filepath.ToSlash(relative))
	})
}

// zipGoMod reads the go.mod of a module zip, the bytes whose hash go.sum
// states for module@version/go.mod.
func zipGoMod(zipPath string, pin moduleVersion) ([]byte, error) {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	want := pin.path + "@" + pin.version + "/go.mod"
	for _, entry := range reader.File {
		if entry.Name != want {
			continue
		}
		if entry.UncompressedSize64 > maxGoModBytes {
			return nil, errors.New("the runtime go.mod exceeds its limit")
		}
		input, err := entry.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(input, maxGoModBytes+1))
		if len(data) > maxGoModBytes {
			err = errors.New("the runtime go.mod exceeds its limit")
		}
		closeErr := input.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		return data, nil
	}
	return nil, fmt.Errorf("module %s %s: the runtime folder %s has no go.mod", pin.path, pin.version, RuntimeFolder)
}

// withRuntimeProxy gives the environment with the runtime proxy before the
// public module proxy. The go command asks the public proxy only for a module
// that the runtime proxy does not hold.
func withRuntimeProxy(env []string, proxy string) ([]string, error) {
	// The go command splits GOPROXY at ',' and '|' before it parses each
	// URL, so these cannot be escaped; the URL form escapes '%', '?' and '#',
	// which the go command would decode or cut.
	if !filepath.IsAbs(proxy) || strings.ContainsAny(proxy, ",|") {
		return nil, fmt.Errorf("the runtime proxy folder %s cannot be named in GOPROXY", proxy)
	}
	location := (&url.URL{Scheme: "file", Path: filepath.ToSlash(proxy)}).String()
	result := make([]string, 0, len(env))
	for _, value := range env {
		if strings.HasPrefix(value, "GOPROXY=") {
			value = "GOPROXY=" + location + "," + ProxySource
		}
		result = append(result, value)
	}
	return result, nil
}

// checkCachedModules enforces ST-BLD-INV03: every module zip in the download
// cache must have the hash that go.sum states for it. It refuses a zip that
// go.sum does not state and a zip whose hash differs, and names the module,
// its version, the stated hash and the found hash. It returns the zips in
// path and version order.
func checkCachedModules(download string, sums map[moduleVersion]string) ([]CachedModule, error) {
	var modules []CachedModule
	err := filepath.WalkDir(download, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".zip") {
			return nil
		}
		relative, err := filepath.Rel(download, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		escapedPath, escapedVersion := path.Split(strings.TrimSuffix(relative, ".zip"))
		if !strings.HasSuffix(escapedPath, "/@v/") {
			return fmt.Errorf("module cache holds a zip outside a module version folder: %s", relative)
		}
		modulePath, err := module.UnescapePath(strings.TrimSuffix(escapedPath, "/@v/"))
		if err != nil {
			return fmt.Errorf("module cache holds a zip with an invalid module path: %s", relative)
		}
		version, err := module.UnescapeVersion(escapedVersion)
		if err != nil {
			return fmt.Errorf("module cache holds a zip with an invalid version: %s", relative)
		}
		stated, ok := sums[moduleVersion{modulePath, version}]
		if !ok {
			return fmt.Errorf("module %s %s is in the cache, but go.sum states no hash for it", modulePath, version)
		}
		found, err := dirhash.HashZip(name, dirhash.Hash1)
		if err != nil {
			return fmt.Errorf("module %s %s: the zip cannot be read", modulePath, version)
		}
		if found != stated {
			return fmt.Errorf("module %s %s differs from go.sum: go.sum states %s, the zip is %s", modulePath, version, stated, found)
		}
		modules = append(modules, CachedModule{Path: modulePath, Version: version, Sum: found})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(modules, func(i, j int) bool {
		if modules[i].Path != modules[j].Path {
			return modules[i].Path < modules[j].Path
		}
		return modules[i].Version < modules[j].Version
	})
	return modules, nil
}

// diagnoseDownload finds the module a failed download stopped at. Go withholds
// its diagnostics, so it downloads each module that go.sum states, one at a
// time and outside the service's module, where only the sum database checks
// it, and compares each zip with go.sum. It names the first module that the
// proxy cannot supply or whose hash differs.
func diagnoseDownload(ctx context.Context, root string, env []string, goTool string, sums map[moduleVersion]string, served *moduleVersion) error {
	keys := make([]moduleVersion, 0, len(sums))
	for key := range sums {
		// The runtime folder already matched go.sum for the pin it serves.
		if served != nil && key == *served {
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].path != keys[j].path {
			return keys[i].path < keys[j].path
		}
		return keys[i].version < keys[j].version
	})
	for _, name := range []string{"home", "tmp", "modules", "cache", "gopath"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			return err
		}
	}
	for _, key := range keys {
		if _, err := capture(ctx, filepath.Join(root, "tmp"), env, goTool, "mod", "download", "-json", key.path+"@"+key.version); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("module download stopped at %s %s: the module proxy cannot supply it", key.path, key.version)
		}
		escapedPath, err := module.EscapePath(key.path)
		if err != nil {
			return err
		}
		escapedVersion, err := module.EscapeVersion(key.version)
		if err != nil {
			return err
		}
		found, err := dirhash.HashZip(filepath.Join(root, "modules", "cache", "download", filepath.FromSlash(escapedPath), "@v", escapedVersion+".zip"), dirhash.Hash1)
		if err != nil {
			return fmt.Errorf("module download stopped at %s %s: the zip cannot be read", key.path, key.version)
		}
		if found != sums[key] {
			return fmt.Errorf("module download stopped at %s %s: go.sum states %s, the zip is %s", key.path, key.version, sums[key], found)
		}
	}
	return errors.New("module download failed, and each module that go.sum states downloads alone; a module that go.sum does not state is missing")
}

// validateModuleCacheRecord checks the recorded cache policy.
func validateModuleCacheRecord(record *ModuleCacheRecord) error {
	if record.Format != moduleCacheFormat || record.GoVersion != GoVersion || !revisionPattern.MatchString(record.SourceRevision) || !hashPattern.MatchString(record.GitSHA256) || record.DependencyProxy != ProxySource {
		return errors.New("module cache record violates the build policy")
	}
	if record.Module != "." && !safePath(record.Module) {
		return errors.New("module cache record violates the build policy")
	}
	if !safePath(record.Target) {
		return errors.New("module cache record violates the build policy")
	}
	if record.DownloadCache.Files < 1 || record.DownloadCache.Bytes < 1 || record.DownloadCache.Bytes > maxBytes || !hashPattern.MatchString(record.DownloadCache.SHA256) {
		return errors.New("module cache record violates the build policy")
	}
	if len(record.Modules) < 1 || len(record.Modules) > 4096 {
		return errors.New("module cache record lists no module, or too many")
	}
	for index, cached := range record.Modules {
		if module.Check(cached.Path, cached.Version) != nil || !validModuleSum(cached.Sum) {
			return fmt.Errorf("module cache record lists an invalid module %s %s", cached.Path, cached.Version)
		}
		if cached.Source != ProxySource && (cached.Source != RuntimeFolderSource || cached.Path != RuntimeModule) {
			return fmt.Errorf("module cache record names the source %q for %s %s; only the runtime library can come from %s", cached.Source, cached.Path, cached.Version, RuntimeFolder)
		}
		if index > 0 {
			previous := record.Modules[index-1]
			if previous.Path > cached.Path || previous.Path == cached.Path && previous.Version >= cached.Version {
				return errors.New("module cache record lists its modules out of order")
			}
		}
	}
	return nil
}

// readModuleCacheRecord reads and validates a cache record file.
func readModuleCacheRecord(recordPath string) (*ModuleCacheRecord, error) {
	data, err := os.ReadFile(recordPath)
	if err != nil || len(data) > 16<<20 {
		return nil, errors.New("cannot read the module cache record")
	}
	var record ModuleCacheRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, errors.New("invalid module cache record")
	}
	canonical, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(data, canonical) {
		return nil, errors.New("module cache record is not canonical")
	}
	if err := validateModuleCacheRecord(&record); err != nil {
		return nil, err
	}
	return &record, nil
}

// loadModuleCache reads the record from a cache directory and verifies the
// download cache inventory against it.
func loadModuleCache(root string) (*ModuleCacheRecord, error) {
	record, err := readModuleCacheRecord(filepath.Join(root, "module-cache.json"))
	if err != nil {
		return nil, err
	}
	actual, _, err := inventory(filepath.Join(root, "cache", "download"), maxFiles, maxBytes)
	if err != nil {
		return nil, err
	}
	if actual != record.DownloadCache {
		return nil, errors.New("module cache differs from its record")
	}
	sums := make(map[moduleVersion]string, len(record.Modules))
	for _, cached := range record.Modules {
		sums[moduleVersion{cached.Path, cached.Version}] = cached.Sum
	}
	modules, err := checkCachedModules(filepath.Join(root, "cache", "download"), sums)
	if err != nil {
		return nil, err
	}
	if len(modules) != len(record.Modules) {
		return nil, errors.New("module cache lacks a module zip that its record lists")
	}
	return record, nil
}

func prepareDownload(options DownloadOptions) (DownloadOptions, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return options, errors.New("module cache download requires Linux amd64")
	}
	if !revisionPattern.MatchString(options.Revision) || (options.Module != "." && !safePath(options.Module)) || !safePath(options.Target) {
		return options, errors.New("module cache download requires a commit, relative module, and relative target")
	}
	for _, pointer := range []*string{&options.Source, &options.Go, &options.Output} {
		if *pointer == "" {
			return options, errors.New("module cache download paths are required")
		}
		absolute, err := filepath.Abs(*pointer)
		if err != nil {
			return options, err
		}
		*pointer = absolute
	}
	var err error
	options.Source, err = filepath.EvalSymlinks(options.Source)
	if err != nil {
		return options, err
	}
	options.Go, err = filepath.EvalSymlinks(options.Go)
	if err != nil {
		return options, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(options.Output))
	if err != nil {
		return options, fmt.Errorf("module cache target %s: its parent folder must already exist", options.Output)
	}
	options.Output = filepath.Join(parent, filepath.Base(options.Output))
	if _, err = os.Lstat(options.Output); !errors.Is(err, os.ErrNotExist) {
		return options, fmt.Errorf("module cache target %s exists; expected a new folder", options.Output)
	}
	sdk := filepath.Dir(filepath.Dir(options.Go))
	if options.Go != filepath.Join(sdk, "bin", "go") {
		return options, fmt.Errorf("--go names %s; expected bin/go of a Go SDK", options.Go)
	}
	if !outside(options.Source, options.Output) {
		return options, fmt.Errorf("module cache target %s is inside the source %s; expected a folder outside it", options.Output, options.Source)
	}
	if !outside(sdk, options.Output) {
		return options, fmt.Errorf("module cache target %s is inside the Go SDK %s; expected a folder outside it", options.Output, sdk)
	}
	return options, nil
}

// copyTree copies a directory tree as data. It refuses links and special
// files, which also rejects extracted module sources with go.sum unchecked.
func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		if relative == "." {
			if err := os.MkdirAll(destination, 0700); err != nil {
				return err
			}
			return nil
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("module cache contains a link or special file")
		}
		input, err := os.Open(name)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, err = io.Copy(output, input)
		if err == nil {
			err = output.Sync()
		}
		closeErr := output.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}

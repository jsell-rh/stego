package service

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/jsell-rh/stego/registry/runtime/query"
)

// The fixed names of the settings of the process environment (ST-SUP-RP14-LC01).
const (
	// DatabaseURLSetting is the connection string of the runtime role, and DatabaseURLFileSetting the absolute path
	// of a file that holds it: one of the two.
	DatabaseURLSetting     = "DATABASE_URL"
	DatabaseURLFileSetting = "DATABASE_URL_FILE"
	// MigrationDatabaseURLSetting is the connection string of the migration job: a login that is the owner role or
	// may SET ROLE to it, and never the runtime role. MigrationDatabaseURLFileSetting is a file that holds it.
	MigrationDatabaseURLSetting     = "STEGO_MIGRATION_DATABASE_URL"
	MigrationDatabaseURLFileSetting = "STEGO_MIGRATION_DATABASE_URL_FILE"
	// IssuerSetting and AudienceSetting are the issuer and the audience that each bearer token must name.
	IssuerSetting   = "STEGO_AUTHN_ISSUER"
	AudienceSetting = "STEGO_AUTHN_AUDIENCE"
	// KeysURLSetting and KeysFileSetting name the key document of the issuer: one of the two, and neither when the
	// declaration states discovery or a public key file.
	KeysURLSetting  = "STEGO_AUTHN_KEYS_URL"
	KeysFileSetting = "STEGO_AUTHN_KEYS_FILE"
	// CAFileSetting is the PEM file of certificate authorities that the verifier adds to the system roots for the
	// discovery document and the key document of the issuer; it never replaces them (ST-SUP-RP13-LC08).
	CAFileSetting = "STEGO_AUTHN_CA_FILE"
	// SpiceDBEndpointSetting is the address of SpiceDB, and SpiceDBKeyFileSetting the file of its preshared key.
	SpiceDBEndpointSetting = "STEGO_SPICEDB_ENDPOINT"
	SpiceDBKeyFileSetting  = "STEGO_SPICEDB_KEY_FILE"
	// SpiceDBInsecureLoopbackSetting set to 1 lets the connection to a loopback SpiceDB address go without TLS, for a
	// local test only; every other address needs verified TLS.
	SpiceDBInsecureLoopbackSetting = "STEGO_SPICEDB_INSECURE_LOOPBACK"
	// CursorKeyFileSetting is the file of the key of the list cursors: at least query.MinCursorKey bytes, the same in
	// every replica.
	CursorKeyFileSetting = "STEGO_QUERY_CURSOR_KEY_FILE"
	// VersionSetting is the version of the release, which each audit row carries.
	VersionSetting = "STEGO_SERVICE_VERSION"
)

// Auth are the settings of the verifier that the declaration states through the settings of its auth provider,
// such as jwt-auth or oidc-auth; an empty one is not stated. A stated setting wins: the environment may only match it.
type Auth struct {
	Issuer, Audience, Header, KeyFile, RoleClaim string
	// Discovery states that the key set comes from the discovery document of the issuer (ST-SUP-RP13-LC21), as
	// oidc-auth states it; the environment then names no key document.
	Discovery bool
	// CAFile is the CA file that the declaration states for the discovery document and the key document.
	CAFile string
	// Algorithms are the declared accepted signing algorithms, and SubjectClaim the declared subject claim; empty
	// gives the verifier's own default, RS256 and sub (ST-SUP-RP13-LC03, LC13).
	Algorithms   []string
	SubjectClaim string
}

// SpiceDB is how the service reaches SpiceDB: its address, its preshared key and whether a loopback address goes
// without TLS.
type SpiceDB struct {
	Endpoint         string
	Key              string
	InsecureLoopback bool
}

// Settings are the settings of a running service, which ReadSettings reads from the process environment.
type Settings struct {
	// DatabaseURL is the connection string of the runtime role.
	DatabaseURL string
	// Issuer and Audience are the issuer and the audience of the bearer tokens.
	Issuer, Audience string
	// KeysURL and KeysFile name the key document of the issuer, and KeyFile the public key file that the
	// declaration states: one of the three, or none when Discovery holds.
	KeysURL, KeysFile, KeyFile string
	// Discovery states that the key set comes from the discovery document of the issuer, and CAFile is the CA file
	// that adds to the system roots for it and for a key URL.
	Discovery bool
	CAFile    string
	// Algorithms and SubjectClaim are the declared accepted algorithms and subject claim, or empty.
	Algorithms   []string
	SubjectClaim string
	// Header and RoleClaim are the token header and the role claim that the declaration states, or empty.
	Header, RoleClaim string
	// SpiceDB is how the service reaches SpiceDB.
	SpiceDB SpiceDB
	// CursorKey is the key of the list cursors.
	CursorKey []byte
	// ServiceVersion is the version of the release, or empty.
	ServiceVersion string
}

// MigrationSettings are the settings of the migration job, which ReadMigrationSettings reads.
type MigrationSettings struct {
	// DatabaseURL is the connection string of the migration job.
	DatabaseURL string
	// SpiceDB is how the job reaches SpiceDB to write the schema.
	SpiceDB SpiceDB
}

// settingError is a missing or malformed setting: it names the setting and never its value.
func settingError(name, problem string) error {
	return fmt.Errorf("service: the setting %s %s", name, problem)
}

// ReadSettings reads the settings of a running service from the process environment by their fixed names, with the
// verifier settings that the declaration states (ST-SUP-RP14-LC01). It refuses a missing or malformed setting, and a
// setting that differs from the stated one, with an error that names the setting and not its value.
func ReadSettings(stated Auth) (Settings, error) {
	var s Settings
	var err error
	if s.DatabaseURL, err = connectionString(DatabaseURLSetting, DatabaseURLFileSetting); err != nil {
		return Settings{}, err
	}
	if s.Issuer, err = declared(IssuerSetting, stated.Issuer); err != nil {
		return Settings{}, err
	}
	if s.Audience, err = declared(AudienceSetting, stated.Audience); err != nil {
		return Settings{}, err
	}
	s.Header, s.RoleClaim = stated.Header, stated.RoleClaim
	s.Algorithms, s.SubjectClaim = slices.Clone(stated.Algorithms), stated.SubjectClaim
	s.KeysURL, s.KeysFile = os.Getenv(KeysURLSetting), os.Getenv(KeysFileSetting)
	if s.CAFile = os.Getenv(CAFileSetting); stated.CAFile != "" {
		if s.CAFile != "" && s.CAFile != stated.CAFile {
			return Settings{}, settingError(CAFileSetting, "differs from the value that the declaration states, which wins")
		}
		s.CAFile = stated.CAFile
	}
	switch {
	case stated.Discovery && stated.KeyFile != "":
		return Settings{}, settingError(KeysURLSetting+" or "+KeysFileSetting, "has no source: the declaration states both discovery and a public key file")
	case stated.Discovery && (s.KeysURL != "" || s.KeysFile != ""):
		return Settings{}, settingError(KeysURLSetting+" or "+KeysFileSetting, "must not be set: the declaration states discovery through the issuer")
	case stated.Discovery:
		s.Discovery = true
	case stated.KeyFile != "" && (s.KeysURL != "" || s.KeysFile != ""):
		return Settings{}, settingError(KeysURLSetting+" or "+KeysFileSetting, "must not be set: the declaration states a public key file")
	case stated.KeyFile != "":
		s.KeyFile = stated.KeyFile
	case (s.KeysURL == "") == (s.KeysFile == ""):
		return Settings{}, settingError(KeysURLSetting+" or "+KeysFileSetting, "must be set, one of the two")
	}
	if s.CAFile != "" && s.KeysURL == "" && !s.Discovery {
		return Settings{}, settingError(CAFileSetting, "is only for a key URL or discovery through the issuer")
	}
	if s.SpiceDB, err = readSpiceDB(); err != nil {
		return Settings{}, err
	}
	if s.CursorKey, err = cursorKey(); err != nil {
		return Settings{}, err
	}
	s.ServiceVersion = os.Getenv(VersionSetting)
	return s, nil
}

// ReadMigrationSettings reads the settings of the migration job from the process environment by their fixed names
// (ST-SUP-RP14-LC01): the connection string of the migration job and SpiceDB.
func ReadMigrationSettings() (MigrationSettings, error) {
	url, err := connectionString(MigrationDatabaseURLSetting, MigrationDatabaseURLFileSetting)
	if err != nil {
		return MigrationSettings{}, err
	}
	spice, err := readSpiceDB()
	if err != nil {
		return MigrationSettings{}, err
	}
	return MigrationSettings{DatabaseURL: url, SpiceDB: spice}, nil
}

// readSpiceDB reads the address of SpiceDB, its preshared key from the key file, with the checks of settingFile,
// and the loopback test mode.
func readSpiceDB() (SpiceDB, error) {
	var s SpiceDB
	var err error
	if s.Endpoint, err = required(SpiceDBEndpointSetting); err != nil {
		return SpiceDB{}, err
	}
	name, err := required(SpiceDBKeyFileSetting)
	if err != nil {
		return SpiceDB{}, err
	}
	key, err := settingFile(SpiceDBKeyFileSetting, name)
	if err != nil {
		return SpiceDB{}, err
	}
	if s.Key = strings.TrimSpace(string(key)); s.Key == "" {
		return SpiceDB{}, settingError(SpiceDBKeyFileSetting, "names an empty key")
	}
	switch os.Getenv(SpiceDBInsecureLoopbackSetting) {
	case "", "0":
	case "1":
		host, _, err := net.SplitHostPort(s.Endpoint)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			return SpiceDB{}, settingError(SpiceDBInsecureLoopbackSetting, "allows no TLS only for a loopback address of "+SpiceDBEndpointSetting)
		}
		s.InsecureLoopback = true
	default:
		return SpiceDB{}, settingError(SpiceDBInsecureLoopbackSetting, "must be 0 or 1")
	}
	return s, nil
}

// cursorKey reads the key of the list cursors from the file of CursorKeyFileSetting, with the checks of
// settingFile.
func cursorKey() ([]byte, error) {
	name, err := required(CursorKeyFileSetting)
	if err != nil {
		return nil, err
	}
	key, err := settingFile(CursorKeyFileSetting, name)
	if err != nil {
		return nil, err
	}
	if len(key) < query.MinCursorKey {
		return nil, settingError(CursorKeyFileSetting, fmt.Sprintf("names a key of fewer than %d bytes", query.MinCursorKey))
	}
	return key, nil
}

// declared gives the value that the declaration states, or the setting when it states none. A setting that is set
// to another value than the stated one is refused: the declaration wins.
func declared(name, stated string) (string, error) {
	value := os.Getenv(name)
	if stated == "" {
		if value == "" {
			return "", settingError(name, "is not set, and the declaration states no value for it")
		}
		return value, nil
	}
	if value != "" && value != stated {
		return "", settingError(name, "differs from the value that the declaration states, which wins")
	}
	return stated, nil
}

// required gives the value of the setting name, or an error that names it when it is empty.
func required(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", settingError(name, "is not set")
	}
	return value, nil
}

// errNoConnectionFile is the refusal of a connection string file that is not a safe regular file.
var errNoConnectionFile = errors.New("names no readable connection string file")

// errNoSettingFile is the refusal of a file of a setting that is not a safe regular file.
var errNoSettingFile = errors.New("names no safe readable file: an absolute path of a regular file of at most 64 KiB that no group or other may write")

// maxSettingFile is the largest file of a setting, in bytes.
const maxSettingFile = 65536

// settingFile reads the file at path that the setting name names, with the checks of a connection string file: an
// absolute path of a regular file of at most 64 KiB, which it opens without waiting on a FIFO, that has no execute
// bit for its owner and that no group or other may write or execute. A refusal names the setting and not the path.
func settingFile(name, path string) ([]byte, error) {
	invalid := settingError(name, errNoSettingFile.Error())
	if !filepath.IsAbs(path) {
		return nil, invalid
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, invalid
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSettingFile || info.Mode().Perm()&0o137 != 0 {
		return nil, invalid
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSettingFile+1))
	if err != nil || len(data) > maxSettingFile {
		return nil, invalid
	}
	return data, nil
}

// connectionString gives the connection string of the setting name, or of the file that the setting fileName
// names: one of the two. The file passes the checks of settingFile and holds one line.
func connectionString(name, fileName string) (string, error) {
	value, file := os.Getenv(name), os.Getenv(fileName)
	if (value == "") == (file == "") {
		return "", settingError(name+" or "+fileName, "must be set, exactly one of the two")
	}
	if file == "" {
		return value, nil
	}
	invalid := settingError(fileName, errNoConnectionFile.Error())
	data, err := settingFile(fileName, file)
	if err != nil {
		return "", invalid
	}
	value = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", invalid
	}
	return value, nil
}

package auditsink

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/jsell-rh/stego/registry/runtime/audit"
)

// The settings of the audit sink, which the worker mode reads from the process environment by these fixed names.
const (
	// URLSetting is the HTTPS URL of the OTLP/HTTP logs endpoint, its full path, such as https://collector/v1/logs.
	// Unset: the default sink, the table of its own in the service database.
	URLSetting = "STEGO_AUDIT_SINK_URL"
	// TokenFileSetting is the absolute path of the file that holds the token, read again on each send.
	TokenFileSetting = "STEGO_AUDIT_SINK_TOKEN_FILE"
	// AuthHeaderSetting names the header that carries the token as it is in the file. Unset: the token goes as
	// "Authorization: Bearer <token>".
	AuthHeaderSetting = "STEGO_AUDIT_SINK_AUTH_HEADER"
	// CAFileSetting is the absolute path of a PEM file of certificate authorities, added to the system roots.
	CAFileSetting = "STEGO_AUDIT_SINK_CA_FILE"
	// ClientCertFileSetting and ClientKeyFileSetting are the absolute paths of the PEM client certificate and key of
	// mutual TLS. Both or neither.
	ClientCertFileSetting = "STEGO_AUDIT_SINK_CLIENT_CERT_FILE"
	ClientKeyFileSetting  = "STEGO_AUDIT_SINK_CLIENT_KEY_FILE"
)

// Settings are the settings of the audit sink.
type Settings struct {
	URL, TokenFile, AuthHeader, CAFile, ClientCertFile, ClientKeyFile string
}

// HTTPS tells whether the settings name an HTTPS endpoint; else the sink is the default table.
func (s Settings) HTTPS() bool { return s.URL != "" }

// ReadSettings reads the settings of the audit sink from the process environment and checks them.
func ReadSettings() (Settings, error) {
	s := Settings{URL: os.Getenv(URLSetting), TokenFile: os.Getenv(TokenFileSetting), AuthHeader: os.Getenv(AuthHeaderSetting),
		CAFile: os.Getenv(CAFileSetting), ClientCertFile: os.Getenv(ClientCertFileSetting), ClientKeyFile: os.Getenv(ClientKeyFileSetting)}
	return s, s.Check()
}

// settingError gives the refusal of one setting: it names the setting and holds no value of it.
func settingError(name, problem string) error {
	return fmt.Errorf("auditsink: the setting %s %s", name, problem)
}

// Check refuses settings that the sink cannot use, with an error that names the setting and holds no value of it:
// a setting of the HTTPS endpoint with no URL; a URL that is not HTTPS, has no host, or holds a user or a fragment;
// no token file and no client certificate; a file path that is not absolute and clean; a client certificate with no
// key or a key with no certificate; an auth header with no token file, or one that is not an HTTP field name or is
// Host, Content-Type or Content-Length.
func (s Settings) Check() error {
	if s.URL == "" {
		for name, value := range map[string]string{TokenFileSetting: s.TokenFile, AuthHeaderSetting: s.AuthHeader, CAFileSetting: s.CAFile,
			ClientCertFileSetting: s.ClientCertFile, ClientKeyFileSetting: s.ClientKeyFile} {
			if value != "" {
				return settingError(name, "is set, but "+URLSetting+" is not: the default sink takes no setting")
			}
		}
		return nil
	}
	u, err := url.Parse(s.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return settingError(URLSetting, "must be an HTTPS URL with a host and no user or fragment")
	}
	for name, value := range map[string]string{TokenFileSetting: s.TokenFile, CAFileSetting: s.CAFile,
		ClientCertFileSetting: s.ClientCertFile, ClientKeyFileSetting: s.ClientKeyFile} {
		if value != "" && (!filepath.IsAbs(value) || filepath.Clean(value) != value) {
			return settingError(name, "must be an absolute clean path")
		}
	}
	if (s.ClientCertFile == "") != (s.ClientKeyFile == "") {
		return settingError(ClientCertFileSetting+" and "+ClientKeyFileSetting, "must be set together")
	}
	if s.TokenFile == "" && s.ClientCertFile == "" {
		return settingError(TokenFileSetting+" or "+ClientCertFileSetting, "must be set: the sink sends to no endpoint without a credential")
	}
	if s.AuthHeader != "" {
		if s.TokenFile == "" {
			return settingError(AuthHeaderSetting, "is set, but "+TokenFileSetting+" is not")
		}
		if !fieldName(s.AuthHeader) {
			return settingError(AuthHeaderSetting, "must be an HTTP field name")
		}
		switch strings.ToLower(s.AuthHeader) {
		case "host", "content-type", "content-length":
			return settingError(AuthHeaderSetting, "must not be Host, Content-Type or Content-Length")
		}
	}
	return nil
}

// fieldName tells whether name is an HTTP field name: a token of RFC 9110.
func fieldName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range []byte(name) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return true
}

// New gives the sink of the settings: the HTTPS sink when they name a URL, else the default sink on db, the worker
// role's pool.
func New(s Settings, db *sql.DB) (audit.Sink, error) {
	if err := s.Check(); err != nil {
		return nil, err
	}
	if s.HTTPS() {
		return NewHTTPSSink(s)
	}
	return NewTableSink(db)
}

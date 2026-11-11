package auditsink

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettings(t *testing.T) {
	t.Run("ST-SUP-RP05-LC17-T01 The sink settings are read by their fixed names, and a bad setting is refused by its name", func(t *testing.T) {
		token := writeFile(t, "token", []byte("s3cret-token"))
		cert, key := writeFile(t, "c.pem", []byte("x")), writeFile(t, "c.key", []byte("x"))
		url := "https://collector.test/v1/logs?key=s3cret-query"
		// A relative path that names the token file, so only the path check refuses it.
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		relative, err := filepath.Rel(wd, token)
		if err != nil {
			t.Fatal(err)
		}
		for name, c := range map[string]struct {
			settings Settings
			want     string
		}{
			"a token file with no URL":              {Settings{TokenFile: token}, TokenFileSetting},
			"a CA file with no URL":                 {Settings{CAFile: "/etc/ca.pem"}, CAFileSetting},
			"an http URL":                           {Settings{URL: "http://collector.test/v1/logs", TokenFile: token}, URLSetting},
			"a URL with a user":                     {Settings{URL: "https://u:p@collector.test/v1/logs", TokenFile: token}, URLSetting},
			"a URL with no host":                    {Settings{URL: "https:///v1/logs", TokenFile: token}, URLSetting},
			"a URL with a fragment":                 {Settings{URL: "https://collector.test/v1/logs#x", TokenFile: token}, URLSetting},
			"no credential":                         {Settings{URL: url}, TokenFileSetting},
			"a relative token file":                 {Settings{URL: url, TokenFile: relative}, TokenFileSetting},
			"a token file path that is not clean":   {Settings{URL: url, TokenFile: "/run/../token"}, TokenFileSetting},
			"a client certificate with no key":      {Settings{URL: url, ClientCertFile: cert}, ClientKeyFileSetting},
			"a client key with no certificate":      {Settings{URL: url, TokenFile: token, ClientKeyFile: key}, ClientCertFileSetting},
			"an auth header with no token file":     {Settings{URL: url, ClientCertFile: cert, ClientKeyFile: key, AuthHeader: "X-Api-Key"}, AuthHeaderSetting},
			"an auth header that is not a field":    {Settings{URL: url, TokenFile: token, AuthHeader: "X Api: Key"}, AuthHeaderSetting},
			"the auth header Host":                  {Settings{URL: url, TokenFile: token, AuthHeader: "host"}, AuthHeaderSetting},
			"the auth header Content-Type":          {Settings{URL: url, TokenFile: token, AuthHeader: "Content-Type"}, AuthHeaderSetting},
			"the auth header Content-Length":        {Settings{URL: url, TokenFile: token, AuthHeader: "content-length"}, AuthHeaderSetting},
			"a CA file that is not PEM":             {Settings{URL: url, TokenFile: token, CAFile: writeFile(t, "ca.pem", []byte("x"))}, CAFileSetting},
			"a client certificate that is not PEM":  {Settings{URL: url, ClientCertFile: cert, ClientKeyFile: key}, ClientCertFileSetting},
			"a token file that does not exist":      {Settings{URL: url, TokenFile: "/nonexistent/token"}, TokenFileSetting},
			"an empty token file":                   {Settings{URL: url, TokenFile: writeFile(t, "empty", nil)}, TokenFileSetting},
			"a token file with a control character": {Settings{URL: url, TokenFile: writeFile(t, "two", []byte("s3cret\nline"))}, TokenFileSetting},
		} {
			_, err := New(c.settings, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s gave %v, want a refusal that names %s", name, err, c.want)
				continue
			}
			for _, secret := range []string{"s3cret", "u:p"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the refusal of %s holds a secret: %v", name, err)
				}
			}
		}

		// The settings come from the environment by their fixed names.
		s := newStandIn(t, nil)
		ca := writeFile(t, "ca.pem", s.ca.pem)
		for name, value := range map[string]string{URLSetting: s.url("/v1/logs"), TokenFileSetting: token, CAFileSetting: ca,
			AuthHeaderSetting: "X-Api-Key", ClientCertFileSetting: "", ClientKeyFileSetting: ""} {
			t.Setenv(name, value)
		}
		read, err := ReadSettings()
		if err != nil || read != (Settings{URL: s.url("/v1/logs"), TokenFile: token, CAFile: ca, AuthHeader: "X-Api-Key"}) {
			t.Fatalf("ReadSettings gave %+v, %v", read, err)
		}
		if sink, err := New(read, nil); err != nil {
			t.Fatal(err)
		} else if _, ok := sink.(*HTTPSSink); !ok {
			t.Fatalf("the settings with a URL gave the sink %T, want the HTTPS sink", sink)
		}
		// With no URL the sink is the default table on the worker role's pool, which it needs.
		if _, err := New(Settings{}, nil); err == nil {
			t.Fatal("the default sink with no database gave no error")
		}
		db, err := sql.Open("pgx", "postgres://127.0.0.1:1/none")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if sink, err := New(Settings{}, db); err != nil {
			t.Fatal(err)
		} else if _, ok := sink.(*TableSink); !ok {
			t.Fatalf("the settings with no URL gave the sink %T, want the default table", sink)
		}
	})
}

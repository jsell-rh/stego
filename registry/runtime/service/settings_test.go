package service

import (
	"strings"
	"testing"
)

// secretValue is in the value of each setting of the test, so a refusal that shows a value shows it.
const secretValue = "s3cret-value"

func TestSettings(t *testing.T) {
	t.Run("ST-SUP-RP14-LC01-T01 Settings come from the process environment by fixed names", func(t *testing.T) {
		dir := t.TempDir()
		spiceKey := writeFile(t, "spicedb.key", []byte(secretValue+"-key\n"))
		cursor := writeFile(t, "cursor.key", []byte(strings.Repeat("c", 32)+secretValue))
		keys := writeFile(t, "keys.json", []byte(`{"keys":[]}`))
		dsnFile := writeFile(t, "dsn", []byte("postgres://rt:"+secretValue+"@db/app\n"))
		migrationFile := writeFile(t, "migration-dsn", []byte("postgres://owner:"+secretValue+"@db/app"))
		full := map[string]string{
			DatabaseURLSetting:          "postgres://rt:" + secretValue + "@db/app",
			MigrationDatabaseURLSetting: "postgres://owner:" + secretValue + "@db/app",
			IssuerSetting:               "https://issuer.example/" + secretValue,
			AudienceSetting:             "orders-" + secretValue,
			KeysURLSetting:              "https://issuer.example/keys/" + secretValue,
			SpiceDBEndpointSetting:      "127.0.0.1:50051",
			SpiceDBKeyFileSetting:       spiceKey,
			CursorKeyFileSetting:        cursor,
			VersionSetting:              "1.2.3",
		}
		set := func(t *testing.T, env map[string]string) {
			for _, name := range []string{DatabaseURLSetting, DatabaseURLFileSetting, MigrationDatabaseURLSetting,
				MigrationDatabaseURLFileSetting, IssuerSetting, AudienceSetting, KeysURLSetting, KeysFileSetting, CAFileSetting,
				SpiceDBEndpointSetting, SpiceDBKeyFileSetting, SpiceDBInsecureLoopbackSetting, CursorKeyFileSetting, VersionSetting} {
				t.Setenv(name, env[name])
			}
		}
		with := func(change map[string]string) map[string]string {
			env := map[string]string{}
			for k, v := range full {
				env[k] = v
			}
			for k, v := range change {
				env[k] = v
			}
			return env
		}

		t.Run("every setting by its value", func(t *testing.T) {
			set(t, with(map[string]string{SpiceDBInsecureLoopbackSetting: "1"}))
			s, err := ReadSettings(Auth{Header: "X-Forwarded-Access-Token", RoleClaim: "realm_access.roles"})
			if err != nil {
				t.Fatal(err)
			}
			m, err := ReadMigrationSettings()
			if err != nil {
				t.Fatal(err)
			}
			if s.DatabaseURL != full[DatabaseURLSetting] || s.Issuer != full[IssuerSetting] || s.Audience != full[AudienceSetting] ||
				s.KeysURL != full[KeysURLSetting] || s.SpiceDB.Endpoint != "127.0.0.1:50051" || s.SpiceDB.Key != secretValue+"-key" ||
				!s.SpiceDB.InsecureLoopback || len(s.CursorKey) != 32+len(secretValue) || s.ServiceVersion != "1.2.3" ||
				s.Header != "X-Forwarded-Access-Token" || s.RoleClaim != "realm_access.roles" || m.DatabaseURL != full[MigrationDatabaseURLSetting] {
				t.Fatalf("the settings are %+v and %+v", s, m)
			}
		})
		t.Run("each file form in place of its value", func(t *testing.T) {
			set(t, with(map[string]string{DatabaseURLSetting: "", DatabaseURLFileSetting: dsnFile, MigrationDatabaseURLSetting: "",
				MigrationDatabaseURLFileSetting: migrationFile, KeysURLSetting: "", KeysFileSetting: keys}))
			s, err := ReadSettings(Auth{})
			if err != nil {
				t.Fatal(err)
			}
			m, err := ReadMigrationSettings()
			if err != nil {
				t.Fatal(err)
			}
			if s.DatabaseURL != full[DatabaseURLSetting] || s.KeysFile != keys || m.DatabaseURL != full[MigrationDatabaseURLSetting] {
				t.Fatalf("the file forms gave %+v and %+v", s, m)
			}
		})
		t.Run("the declared verifier settings win", func(t *testing.T) {
			set(t, with(map[string]string{IssuerSetting: "", KeysURLSetting: ""}))
			s, err := ReadSettings(Auth{Issuer: "https://declared.example", KeyFile: "/etc/keys/issuer.pem"})
			if err != nil || s.Issuer != "https://declared.example" || s.KeyFile != "/etc/keys/issuer.pem" {
				t.Fatalf("the declared settings gave %+v, %v", s, err)
			}
			set(t, with(nil))
			if _, err := ReadSettings(Auth{Issuer: "https://declared.example"}); err == nil || !strings.Contains(err.Error(), IssuerSetting) {
				t.Fatalf("an issuer setting that differs from the declared one gave %v", err)
			}
			if _, err := ReadSettings(Auth{KeyFile: "/etc/keys/issuer.pem"}); err == nil || !strings.Contains(err.Error(), KeysURLSetting) {
				t.Fatalf("a keys setting beside a declared key file gave %v", err)
			}
		})
		t.Run("ST-SUP-RP14-LC01-T02 A declaration with discovery takes no key document and the CA file", func(t *testing.T) {
			ca := writeFile(t, "ca.pem", []byte("CA"))
			oidc := Auth{Issuer: "https://declared.example/realms/r1", Discovery: true, Algorithms: []string{"RS256", "ES256"},
				SubjectClaim: "oid"}
			set(t, with(map[string]string{IssuerSetting: "", KeysURLSetting: "", CAFileSetting: ca}))
			s, err := ReadSettings(oidc)
			if err != nil || !s.Discovery || s.KeysURL != "" || s.KeysFile != "" || s.CAFile != ca || s.Issuer != oidc.Issuer ||
				len(s.Algorithms) != 2 || s.SubjectClaim != "oid" {
				t.Fatalf("the settings of a declaration with discovery are %+v, %v", s, err)
			}
			stated := oidc
			stated.CAFile = "/etc/stego/declared-ca.pem"
			set(t, with(map[string]string{IssuerSetting: "", KeysURLSetting: ""}))
			if s, err := ReadSettings(stated); err != nil || s.CAFile != stated.CAFile {
				t.Fatalf("the declared CA file gave %+v, %v", s, err)
			}
			for name, c := range map[string]struct {
				env   map[string]string
				auth  Auth
				names string
			}{
				"a key URL beside discovery":       {map[string]string{IssuerSetting: ""}, oidc, KeysURLSetting},
				"a key file beside discovery":      {map[string]string{IssuerSetting: "", KeysURLSetting: "", KeysFileSetting: "/k-" + secretValue}, oidc, KeysFileSetting},
				"discovery and a public key file":  {map[string]string{IssuerSetting: "", KeysURLSetting: ""}, Auth{Issuer: oidc.Issuer, Discovery: true, KeyFile: "/k.pem"}, KeysURLSetting},
				"a CA file other than the stated":  {map[string]string{IssuerSetting: "", KeysURLSetting: "", CAFileSetting: "/other-" + secretValue}, stated, CAFileSetting},
				"a CA file beside a key file only": {map[string]string{KeysURLSetting: "", KeysFileSetting: keys, CAFileSetting: ca}, Auth{}, CAFileSetting},
			} {
				t.Run(name, func(t *testing.T) {
					set(t, with(c.env))
					_, err := ReadSettings(c.auth)
					if err == nil || !strings.Contains(err.Error(), c.names) || strings.Contains(err.Error(), secretValue) {
						t.Fatalf("%s gave %v, want a refusal that names %s and holds no value", name, err, c.names)
					}
				})
			}
		})
		refused := map[string]map[string]string{
			DatabaseURLSetting:             {DatabaseURLSetting: ""},
			IssuerSetting:                  {IssuerSetting: ""},
			AudienceSetting:                {AudienceSetting: ""},
			KeysURLSetting:                 {KeysURLSetting: ""},
			SpiceDBEndpointSetting:         {SpiceDBEndpointSetting: ""},
			SpiceDBKeyFileSetting:          {SpiceDBKeyFileSetting: ""},
			CursorKeyFileSetting:           {CursorKeyFileSetting: ""},
			DatabaseURLFileSetting:         {DatabaseURLSetting: "", DatabaseURLFileSetting: dir + "/missing-" + secretValue},
			SpiceDBInsecureLoopbackSetting: {SpiceDBInsecureLoopbackSetting: "yes-" + secretValue},
		}
		t.Run("refused: no TLS to an address that is not loopback", func(t *testing.T) {
			set(t, with(map[string]string{SpiceDBInsecureLoopbackSetting: "1", SpiceDBEndpointSetting: "spicedb.example:50051"}))
			if _, err := ReadSettings(Auth{}); err == nil || !strings.Contains(err.Error(), SpiceDBInsecureLoopbackSetting) {
				t.Fatalf("no TLS to a remote SpiceDB gave %v", err)
			}
		})
		short := writeFile(t, "short.key", []byte(secretValue))
		refused[CursorKeyFileSetting+" short"] = map[string]string{CursorKeyFileSetting: short}
		for name, change := range refused {
			t.Run("refused without "+name, func(t *testing.T) {
				set(t, with(change))
				_, err := ReadSettings(Auth{})
				setting, _, _ := strings.Cut(name, " ")
				if err == nil || !strings.Contains(err.Error(), setting) || strings.Contains(err.Error(), secretValue) {
					t.Fatalf("the settings with %s missing or malformed gave %v, want a refusal that names it and holds no value", name, err)
				}
			})
		}
		t.Run("refused without the migration database URL", func(t *testing.T) {
			set(t, with(map[string]string{MigrationDatabaseURLSetting: ""}))
			_, err := ReadMigrationSettings()
			if err == nil || !strings.Contains(err.Error(), MigrationDatabaseURLSetting) || strings.Contains(err.Error(), secretValue) {
				t.Fatalf("the migration settings with no URL gave %v", err)
			}
		})
	})
}

package kafkaoutbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allSettings are the names of every setting, which each case of the settings test clears first.
var allSettings = []string{BrokersSetting, BrokersFileSetting, BrokerHostsSetting, BrokerHostsFileSetting, TopicSetting, TopicFileSetting, CAFileSetting,
	ClientCertFileSetting, ClientKeyFileSetting, SASLMechanismSetting, UsernameSetting, UsernameFileSetting,
	PasswordFileSetting, TokenFileSetting, InsecureLoopbackSetting}

func TestSettings(t *testing.T) {
	t.Run("ST-SUP-RP01-LC32-T01 A setting the destination cannot use is refused by its name, with no secret", func(t *testing.T) {
		dir := t.TempDir()
		write := func(name, content string) string {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}
		const secret = "s3cret-value-7f1c"
		brokersFile := write("brokers", "kafka-1.example:9093,kafka-2.example:9093\n")
		topicFile := write("topic", "orders\n")
		userFile := write("user", "svc-orders\n")
		passwordFile := write("password", secret+"\n")
		secretFile := write("secret-broker", secret+":9093\n")
		hostsFile := write("hosts", "127.0.0.1,::1\n")
		cases := []struct {
			name, refused string
			env           map[string]string
			want          Settings
		}{
			{name: "SCRAM from the file forms", env: map[string]string{BrokersFileSetting: brokersFile, TopicFileSetting: topicFile,
				SASLMechanismSetting: MechanismSCRAMSHA512, UsernameFileSetting: userFile, PasswordFileSetting: passwordFile},
				want: Settings{Brokers: []string{"kafka-1.example:9093", "kafka-2.example:9093"}, Topic: "orders",
					Mechanism: MechanismSCRAMSHA512, Username: "svc-orders", PasswordFile: passwordFile}},
			{name: "OAUTHBEARER and a client certificate", env: map[string]string{BrokersSetting: "k:9093", SASLMechanismSetting: MechanismOAuthBearer,
				TokenFileSetting: "/run/token", ClientCertFileSetting: "/run/c.pem", ClientKeyFileSetting: "/run/k.pem", CAFileSetting: "/run/ca.pem"},
				want: Settings{Brokers: []string{"k:9093"}, Mechanism: MechanismOAuthBearer, TokenFile: "/run/token",
					ClientCertFile: "/run/c.pem", ClientKeyFile: "/run/k.pem", CAFile: "/run/ca.pem"}},
			{name: "loopback without TLS", env: map[string]string{BrokersSetting: "127.0.0.1:9092,[::1]:9092", InsecureLoopbackSetting: "1"},
				want: Settings{Brokers: []string{"127.0.0.1:9092", "[::1]:9092"}, InsecureLoopback: true}},
			{name: "a broker host list from its file", env: map[string]string{BrokersSetting: "127.0.0.1:9092", InsecureLoopbackSetting: "1",
				BrokerHostsFileSetting: hostsFile}, want: Settings{Brokers: []string{"127.0.0.1:9092"}, BrokerHosts: []string{"127.0.0.1", "::1"}, InsecureLoopback: true}},
			{name: "a seed outside the broker host list", refused: BrokersSetting, env: map[string]string{BrokersSetting: "k:9093",
				BrokerHostsSetting: "j", ClientCertFileSetting: "/c", ClientKeyFileSetting: "/k"}},
			{name: "a broker host with a port", refused: BrokerHostsSetting, env: map[string]string{BrokersSetting: "k:9093",
				BrokerHostsSetting: "k,k:9093", ClientCertFileSetting: "/c", ClientKeyFileSetting: "/k"}},
			{name: "no broker", refused: BrokersSetting, env: map[string]string{SASLMechanismSetting: MechanismOAuthBearer, TokenFileSetting: "/run/token"}},
			{name: "a value and its file", refused: BrokersSetting + " and " + BrokersFileSetting,
				env: map[string]string{BrokersSetting: "k:9093", BrokersFileSetting: brokersFile}},
			{name: "a relative value file", refused: BrokersFileSetting, env: map[string]string{BrokersFileSetting: "brokers"}},
			{name: "a missing value file", refused: TopicFileSetting, env: map[string]string{BrokersSetting: "k:9093", TopicFileSetting: dir + "/none"}},
			{name: "a broker with no port", refused: BrokersSetting, env: map[string]string{BrokersSetting: "kafka-1.example"}},
			{name: "a broker from a secret file", refused: InsecureLoopbackSetting, env: map[string]string{BrokersFileSetting: secretFile, InsecureLoopbackSetting: "1"}},
			{name: "a broker twice", refused: BrokersSetting, env: map[string]string{BrokersSetting: "k:9093,k:9093"}},
			{name: "seventeen brokers", refused: BrokersSetting, env: map[string]string{BrokersSetting: strings.Repeat("k:1,", 16) + "k:17"}},
			{name: "an internal topic", refused: TopicSetting, env: map[string]string{BrokersSetting: "k:9093", TopicSetting: "__consumer_offsets"}},
			{name: "a topic with a slash", refused: TopicSetting, env: map[string]string{BrokersSetting: "k:9093", TopicSetting: "a/b"}},
			{name: "a relative CA file", refused: CAFileSetting, env: map[string]string{BrokersSetting: "k:9093", CAFileSetting: "ca.pem",
				ClientCertFileSetting: "/c", ClientKeyFileSetting: "/k"}},
			{name: "a certificate with no key", refused: ClientCertFileSetting + " and " + ClientKeyFileSetting,
				env: map[string]string{BrokersSetting: "k:9093", ClientCertFileSetting: "/c"}},
			{name: "an unknown mechanism", refused: SASLMechanismSetting, env: map[string]string{BrokersSetting: "k:9093", SASLMechanismSetting: "PLAIN"}},
			{name: "a token with no mechanism", refused: TokenFileSetting, env: map[string]string{BrokersSetting: "k:9093", TokenFileSetting: "/t",
				ClientCertFileSetting: "/c", ClientKeyFileSetting: "/k"}},
			{name: "SCRAM with no password", refused: PasswordFileSetting, env: map[string]string{BrokersSetting: "k:9093",
				SASLMechanismSetting: MechanismSCRAMSHA512, UsernameSetting: "u"}},
			{name: "SCRAM with no user", refused: UsernameSetting, env: map[string]string{BrokersSetting: "k:9093",
				SASLMechanismSetting: MechanismSCRAMSHA512, PasswordFileSetting: passwordFile}},
			{name: "OAUTHBEARER with a password", refused: UsernameSetting + " and " + PasswordFileSetting, env: map[string]string{BrokersSetting: "k:9093",
				SASLMechanismSetting: MechanismOAuthBearer, TokenFileSetting: "/t", PasswordFileSetting: passwordFile}},
			{name: "TLS with no credential", refused: ClientCertFileSetting + " or " + SASLMechanismSetting, env: map[string]string{BrokersSetting: "k:9093"}},
			{name: "plaintext to a host name", refused: InsecureLoopbackSetting, env: map[string]string{BrokersSetting: "localhost:9092", InsecureLoopbackSetting: "1"}},
			{name: "plaintext to another host", refused: InsecureLoopbackSetting, env: map[string]string{BrokersSetting: "10.0.0.7:9092", InsecureLoopbackSetting: "1"}},
			{name: "plaintext with SASL", refused: InsecureLoopbackSetting, env: map[string]string{BrokersSetting: "127.0.0.1:9092", InsecureLoopbackSetting: "1",
				SASLMechanismSetting: MechanismSCRAMSHA512, UsernameSetting: "u", PasswordFileSetting: passwordFile}},
			{name: "a loopback mode that is not 0 or 1", refused: InsecureLoopbackSetting, env: map[string]string{BrokersSetting: "127.0.0.1:9092", InsecureLoopbackSetting: "yes"}},
		}
		for _, c := range cases {
			for _, name := range allSettings {
				t.Setenv(name, "")
			}
			for name, v := range c.env {
				t.Setenv(name, v)
			}
			got, err := ReadSettings()
			if c.refused == "" {
				if err != nil {
					t.Errorf("%s: refused: %v", c.name, err)
					continue
				}
				if strings.Join(got.Brokers, ",") != strings.Join(c.want.Brokers, ",") || got.Topic != c.want.Topic ||
					got.Mechanism != c.want.Mechanism || got.Username != c.want.Username || got.PasswordFile != c.want.PasswordFile ||
					got.TokenFile != c.want.TokenFile || got.CAFile != c.want.CAFile || got.ClientCertFile != c.want.ClientCertFile ||
					got.ClientKeyFile != c.want.ClientKeyFile || got.InsecureLoopback != c.want.InsecureLoopback ||
					strings.Join(got.BrokerHosts, ",") != strings.Join(c.want.BrokerHosts, ",") {
					t.Errorf("%s: read %+v, want %+v", c.name, got, c.want)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s: taken, want a refusal that names %s", c.name, c.refused)
				continue
			}
			if !strings.Contains(err.Error(), "the setting "+c.refused+" ") {
				t.Errorf("%s: %q does not name %s", c.name, err, c.refused)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), dir) {
				t.Errorf("%s: %q holds a value", c.name, err)
			}
		}
	})
	t.Run("ST-SUP-RP01-LC32-T10 A broker host is compared in ASCII only, and a host that is not ASCII is refused", func(t *testing.T) {
		const kelvin = "\u212aafka.example" // K is the Kelvin sign, which a Unicode case fold makes k
		credential := Settings{ClientCertFile: "/c", ClientKeyFile: "/k"}
		for _, c := range []struct {
			name, refused string
			s             Settings
		}{
			{name: "a host list with the Kelvin sign", refused: BrokerHostsSetting, s: Settings{Brokers: []string{"kafka.example:9093"}, BrokerHosts: []string{kelvin}}},
			{name: "a seed with the Kelvin sign", refused: BrokersSetting, s: Settings{Brokers: []string{kelvin + ":9093"}}},
			{name: "a seed with the Kelvin sign and a list of its ASCII form", refused: BrokersSetting, s: Settings{Brokers: []string{kelvin + ":9093"}, BrokerHosts: []string{"kafka.example"}}},
			{name: "an ASCII host in another case", s: Settings{Brokers: []string{"KAFKA.example:9093"}, BrokerHosts: []string{"kafka.EXAMPLE"}}},
		} {
			c.s.ClientCertFile, c.s.ClientKeyFile = credential.ClientCertFile, credential.ClientKeyFile
			err := c.s.Check()
			switch {
			case c.refused == "" && err != nil:
				t.Errorf("%s: refused: %v", c.name, err)
			case c.refused != "" && (err == nil || !strings.Contains(err.Error(), "the setting "+c.refused+" ")):
				t.Errorf("%s: %v, want a refusal that names %s", c.name, err, c.refused)
			}
		}
		// A broker that names a host in its metadata gets no connection when the host is not ASCII, also when its fold
		// is in the list; an ASCII host of the list in another case is dialed.
		dialer := &brokerDialer{loopbackOnly: true, hosts: map[string]bool{"kafka.example": true, "localhost": true}}
		for _, host := range []string{kelvin, "kafk\u0430.example"} {
			if _, err := dialer.dial(context.Background(), "tcp", host+":9092"); !errors.Is(err, errBrokerRefused) {
				t.Errorf("the broker host %q gave %v, want %v", host, err, errBrokerRefused)
			}
		}
		if _, err := dialer.dial(context.Background(), "tcp", "LOCALHOST:1"); errors.Is(err, errBrokerRefused) {
			t.Errorf("the ASCII host LOCALHOST of the list in another case was refused: %v", err)
		}
	})
	t.Run("ST-SUP-RP01-LC32-T11 A secret file with an execute bit, that its group may write or that others may use is refused by its name", func(t *testing.T) {
		dir := t.TempDir()
		const secret = "s3cret-mode-9a2e"
		for _, c := range []struct {
			mode os.FileMode
			ok   bool
		}{{0o400, true}, {0o600, true}, {0o640, true}, {0o440, true}, {0o644, false}, {0o604, false}, {0o700, false}, {0o620, false}, {0o602, false}, {0o610, false}, {0o666, false}} {
			path := filepath.Join(dir, "password-"+c.mode.String())
			if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, c.mode); err != nil {
				t.Fatal(err)
			}
			got, err := readSecret(PasswordFileSetting, path)
			switch {
			case c.ok && (err != nil || got != secret):
				t.Errorf("a password file of mode %v gave %q and %v, want the password", c.mode, got, err)
			case !c.ok && (err == nil || !strings.Contains(err.Error(), "the setting "+PasswordFileSetting+" ") || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), dir)):
				t.Errorf("a password file of mode %v gave %v, want a refusal that names %s and holds neither the password nor the path", c.mode, err, PasswordFileSetting)
			}
		}
		// The client key of mutual TLS is a secret file too: New refuses a key that its group or others may write.
		cert, key := filepath.Join(dir, "client.pem"), filepath.Join(dir, "client-key.pem")
		for _, f := range []string{cert, key} {
			if err := os.WriteFile(f, []byte("not read"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(key, 0o622); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Settings{Brokers: []string{"k:9093"}, ClientCertFile: cert, ClientKeyFile: key}, nil); err == nil ||
			!strings.Contains(err.Error(), "the setting "+ClientKeyFileSetting+" ") {
			t.Errorf("a client key that others may write gave %v, want a refusal that names %s", err, ClientKeyFileSetting)
		}
	})
	t.Run("ST-SUP-RP01-LC32-T12 A CA file or a file of a setting that its group or others may write is refused by its name", func(t *testing.T) {
		dir := t.TempDir()
		file := func(name, content string, mode os.FileMode) string {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			return path
		}
		for _, c := range []struct {
			name, refused string
			env           map[string]string
		}{
			{"a brokers file of mode 0620", BrokersFileSetting, map[string]string{BrokersFileSetting: file("brokers-620", "127.0.0.1:9092", 0o620), InsecureLoopbackSetting: "1"}},
			{"a brokers file of mode 0602", BrokersFileSetting, map[string]string{BrokersFileSetting: file("brokers-602", "127.0.0.1:9092", 0o602), InsecureLoopbackSetting: "1"}},
			{"a broker host list file of mode 0666", BrokerHostsFileSetting, map[string]string{BrokersSetting: "127.0.0.1:9092", InsecureLoopbackSetting: "1",
				BrokerHostsFileSetting: file("hosts-666", "127.0.0.1", 0o666)}},
			{"a brokers file of mode 0644", "", map[string]string{BrokersFileSetting: file("brokers-644", "127.0.0.1:9092", 0o644), InsecureLoopbackSetting: "1"}},
		} {
			for _, name := range allSettings {
				t.Setenv(name, "")
			}
			for name, v := range c.env {
				t.Setenv(name, v)
			}
			_, err := ReadSettings()
			switch {
			case c.refused == "" && err != nil:
				t.Errorf("%s: refused: %v", c.name, err)
			case c.refused != "" && (err == nil || !strings.Contains(err.Error(), "the setting "+c.refused+" ") || strings.Contains(err.Error(), dir)):
				t.Errorf("%s: %v, want a refusal that names %s and not the path", c.name, err, c.refused)
			}
		}
		// The CA file and the client certificate are read when the destination is made.
		ca := file("ca-666", "not read", 0o666)
		if _, err := New(Settings{Brokers: []string{"k:9093"}, CAFile: ca, Mechanism: MechanismOAuthBearer, TokenFile: "/run/token"}, nil); err == nil ||
			!strings.Contains(err.Error(), "the setting "+CAFileSetting+" ") {
			t.Errorf("a CA file of mode 0666 gave %v, want a refusal that names %s", err, CAFileSetting)
		}
	})
}

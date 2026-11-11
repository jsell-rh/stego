package kafkaoutbox

import (
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
}

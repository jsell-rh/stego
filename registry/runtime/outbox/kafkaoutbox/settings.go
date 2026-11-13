package kafkaoutbox

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// The settings of the Kafka destination, which the worker mode reads from the process environment by these fixed
// names (ST-SUP-RP01-LC32). A setting that is not a secret has a _FILE form that names a file which holds its value:
// one of the two, never both. A secret has only its file form, and the destination reads that file again at each
// authentication.
const (
	// BrokersSetting is the comma-separated list of the seed brokers, each host:port, 1 to 16.
	BrokersSetting     = "STEGO_KAFKA_BROKERS"
	BrokersFileSetting = "STEGO_KAFKA_BROKERS_FILE"
	// BrokerHostsSetting is the comma-separated list of the hosts that the destination may dial, at most 64: the
	// seed brokers and every broker that a broker names in its metadata. Unset: the destination dials each broker that
	// a broker names, as a Kafka client does, held only to verified TLS, or to loopback in the insecure loopback mode.
	BrokerHostsSetting     = "STEGO_KAFKA_BROKER_HOSTS"
	BrokerHostsFileSetting = "STEGO_KAFKA_BROKER_HOSTS_FILE"
	// TopicSetting is the declared topic of every message. Unset: each message goes to the topic of its event type,
	// the kind of its outbox message.
	TopicSetting     = "STEGO_KAFKA_TOPIC"
	TopicFileSetting = "STEGO_KAFKA_TOPIC_FILE"
	// CAFileSetting is the absolute path of a PEM file of certificate authorities, added to the system roots.
	CAFileSetting = "STEGO_KAFKA_CA_FILE"
	// ClientCertFileSetting and ClientKeyFileSetting are the absolute paths of the PEM client certificate and key of
	// mutual TLS. Both or neither.
	ClientCertFileSetting = "STEGO_KAFKA_CLIENT_CERT_FILE"
	ClientKeyFileSetting  = "STEGO_KAFKA_CLIENT_KEY_FILE"
	// SASLMechanismSetting is the SASL mechanism: SCRAM-SHA-512 or OAUTHBEARER. Unset: no SASL.
	SASLMechanismSetting = "STEGO_KAFKA_SASL_MECHANISM"
	// UsernameSetting is the SCRAM user name.
	UsernameSetting     = "STEGO_KAFKA_USERNAME"
	UsernameFileSetting = "STEGO_KAFKA_USERNAME_FILE"
	// PasswordFileSetting is the absolute path of the file that holds the SCRAM password.
	PasswordFileSetting = "STEGO_KAFKA_PASSWORD_FILE"
	// TokenFileSetting is the absolute path of the file that holds the OAUTHBEARER token.
	TokenFileSetting = "STEGO_KAFKA_TOKEN_FILE"
	// InsecureLoopbackSetting set to 1 lets the destination talk to loopback brokers without TLS, for a local test
	// only; every other broker needs verified TLS.
	InsecureLoopbackSetting = "STEGO_KAFKA_INSECURE_LOOPBACK"
)

// The SASL mechanisms that the destination takes.
const (
	MechanismSCRAMSHA512 = "SCRAM-SHA-512"
	MechanismOAuthBearer = "OAUTHBEARER"
)

// MaxBrokers is the most seed brokers of the settings.
const MaxBrokers = 16

// MaxBrokerHosts is the most hosts of the broker host list.
const MaxBrokerHosts = 64

// maxValue is the largest value file and secret file that the destination reads.
const maxValue = 16 << 10

// maxPEM is the largest PEM file that the destination reads.
const maxPEM = 1 << 20

// topicPattern is a Kafka topic name. The settings refuse ".", ".." and a name that starts with "__", which Kafka keeps
// for its internal topics.
var topicPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,249}$`)

// Settings are the settings of the Kafka destination.
type Settings struct {
	// Brokers are the seed brokers, each host:port.
	Brokers []string
	// BrokerHosts are the hosts that the destination may dial, or empty for each broker that a broker names.
	BrokerHosts []string
	// Topic is the declared topic, or empty for the topic of each message's event type.
	Topic string
	// CAFile, ClientCertFile and ClientKeyFile are the absolute paths of the TLS files.
	CAFile, ClientCertFile, ClientKeyFile string
	// Mechanism is the SASL mechanism, or empty for none.
	Mechanism string
	// Username is the SCRAM user name, and PasswordFile the file of its password.
	Username, PasswordFile string
	// TokenFile is the file of the OAUTHBEARER token.
	TokenFile string
	// InsecureLoopback lets the destination talk to loopback brokers without TLS.
	InsecureLoopback bool
}

// settingError gives the refusal of one setting: it names the setting and holds no value of it.
func settingError(name, problem string) error {
	return fmt.Errorf("kafkaoutbox: the setting %s %s", name, problem)
}

// ReadSettings reads the settings of the Kafka destination from the process environment, each value from its
// setting or from the file of its _FILE form, and checks them.
func ReadSettings() (Settings, error) {
	var s Settings
	brokers, err := value(BrokersSetting, BrokersFileSetting)
	if err != nil {
		return Settings{}, err
	}
	if brokers != "" {
		s.Brokers = strings.Split(brokers, ",")
	}
	hosts, err := value(BrokerHostsSetting, BrokerHostsFileSetting)
	if err != nil {
		return Settings{}, err
	}
	if hosts != "" {
		s.BrokerHosts = strings.Split(hosts, ",")
	}
	if s.Topic, err = value(TopicSetting, TopicFileSetting); err != nil {
		return Settings{}, err
	}
	if s.Username, err = value(UsernameSetting, UsernameFileSetting); err != nil {
		return Settings{}, err
	}
	s.CAFile, s.ClientCertFile, s.ClientKeyFile = os.Getenv(CAFileSetting), os.Getenv(ClientCertFileSetting), os.Getenv(ClientKeyFileSetting)
	s.Mechanism, s.PasswordFile, s.TokenFile = os.Getenv(SASLMechanismSetting), os.Getenv(PasswordFileSetting), os.Getenv(TokenFileSetting)
	switch os.Getenv(InsecureLoopbackSetting) {
	case "", "0":
	case "1":
		s.InsecureLoopback = true
	default:
		return Settings{}, settingError(InsecureLoopbackSetting, "must be 0 or 1")
	}
	return s, s.Check()
}

// value reads one setting that is not a secret: from name, or from the file that fileName names; never both.
func value(name, fileName string) (string, error) {
	v, file := os.Getenv(name), os.Getenv(fileName)
	if v != "" && file != "" {
		return "", settingError(name+" and "+fileName, "are both set: set one of the two")
	}
	if file == "" {
		return v, nil
	}
	if !cleanPath(file) {
		return "", settingError(fileName, "must be an absolute clean path")
	}
	data, err := readFile(file, maxValue)
	if err != nil {
		return "", settingError(fileName, "must name a readable regular file of at most 16 KiB")
	}
	v = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if v == "" || strings.ContainsFunc(v, control) {
		return "", settingError(fileName, "must name a file of one value with no control character")
	}
	return v, nil
}

// Check refuses settings that the destination cannot use, with an error that names the setting and holds no value of
// it: no broker, more than MaxBrokers, a broker that is not host:port or is named twice; a topic that Kafka does not
// take or keeps for itself; a file path that is not absolute and clean; a client certificate with no key or a key
// with no certificate; an unknown SASL mechanism, or a SASL setting that its mechanism does not take; TLS with no
// client certificate and no SASL, as a broker that knows no client is not trusted; and the insecure loopback mode
// with a broker that is not a loopback IP address, or with a TLS or SASL setting.
func (s Settings) Check() error {
	if len(s.Brokers) == 0 {
		return settingError(BrokersSetting, "must be set: the destination needs at least one broker")
	}
	if len(s.Brokers) > MaxBrokers {
		return settingError(BrokersSetting, fmt.Sprintf("names more than %d brokers", MaxBrokers))
	}
	seen := map[string]bool{}
	for _, broker := range s.Brokers {
		host, port, err := net.SplitHostPort(broker)
		number, portErr := strconv.Atoi(port)
		if _, ascii := asciiLower(host); err != nil || portErr != nil || number < 1 || number > 65535 || host == "" ||
			len(host) > 253 || !ascii || strings.ContainsAny(host, "/\\@ ,") || strings.ContainsFunc(host, control) {
			return settingError(BrokersSetting, "must be a comma-separated list of host:port")
		}
		if seen[broker] {
			return settingError(BrokersSetting, "names one broker twice")
		}
		seen[broker] = true
	}
	if len(s.BrokerHosts) > MaxBrokerHosts {
		return settingError(BrokerHostsSetting, fmt.Sprintf("names more than %d hosts", MaxBrokerHosts))
	}
	allowed := map[string]bool{}
	for _, host := range s.BrokerHosts {
		lower, ascii := asciiLower(host)
		if host == "" || len(host) > 253 || !ascii || strings.ContainsAny(host, "/\\@ ,[]") || strings.ContainsFunc(host, control) ||
			strings.Contains(host, ":") && net.ParseIP(host) == nil {
			return settingError(BrokerHostsSetting, "must be a comma-separated list of ASCII host names or IP addresses with no port")
		}
		allowed[lower] = true
	}
	if len(allowed) > 0 {
		for _, broker := range s.Brokers {
			host, _, _ := net.SplitHostPort(broker)
			if lower, _ := asciiLower(host); !allowed[lower] {
				return settingError(BrokersSetting, "names a host that "+BrokerHostsSetting+" does not hold")
			}
		}
	}
	if s.Topic != "" && !validTopic(s.Topic) {
		return settingError(TopicSetting, "must be a Kafka topic name of [a-zA-Z0-9._-], at most 249 characters, not . or .. and not starting with __")
	}
	for name, path := range map[string]string{CAFileSetting: s.CAFile, ClientCertFileSetting: s.ClientCertFile,
		ClientKeyFileSetting: s.ClientKeyFile, PasswordFileSetting: s.PasswordFile, TokenFileSetting: s.TokenFile} {
		if path != "" && !cleanPath(path) {
			return settingError(name, "must be an absolute clean path")
		}
	}
	if (s.ClientCertFile == "") != (s.ClientKeyFile == "") {
		return settingError(ClientCertFileSetting+" and "+ClientKeyFileSetting, "must be set together")
	}
	switch s.Mechanism {
	case "":
		for name, v := range map[string]string{UsernameSetting: s.Username, PasswordFileSetting: s.PasswordFile, TokenFileSetting: s.TokenFile} {
			if v != "" {
				return settingError(name, "is set, but "+SASLMechanismSetting+" is not")
			}
		}
	case MechanismSCRAMSHA512:
		if s.Username == "" || len(s.Username) > 255 || !utf8.ValidString(s.Username) || strings.ContainsFunc(s.Username, control) {
			return settingError(UsernameSetting, "must be set for SCRAM-SHA-512: a user name of at most 255 bytes with no control character")
		}
		if s.PasswordFile == "" {
			return settingError(PasswordFileSetting, "must be set for SCRAM-SHA-512")
		}
		if s.TokenFile != "" {
			return settingError(TokenFileSetting, "is set, but SCRAM-SHA-512 takes no token")
		}
	case MechanismOAuthBearer:
		if s.TokenFile == "" {
			return settingError(TokenFileSetting, "must be set for OAUTHBEARER")
		}
		if s.Username != "" || s.PasswordFile != "" {
			return settingError(UsernameSetting+" and "+PasswordFileSetting, "must not be set for OAUTHBEARER")
		}
	default:
		return settingError(SASLMechanismSetting, "must be SCRAM-SHA-512 or OAUTHBEARER")
	}
	if s.InsecureLoopback {
		for _, broker := range s.Brokers {
			host, _, _ := net.SplitHostPort(broker)
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				return settingError(InsecureLoopbackSetting, "allows no TLS only for brokers at a loopback IP address")
			}
		}
		if s.CAFile != "" || s.ClientCertFile != "" || s.Mechanism != "" {
			return settingError(InsecureLoopbackSetting, "takes no TLS file and no SASL mechanism")
		}
		return nil
	}
	if s.ClientCertFile == "" && s.Mechanism == "" {
		return settingError(ClientCertFileSetting+" or "+SASLMechanismSetting, "must be set: the destination sends to no broker without a credential")
	}
	return nil
}

// validTopic tells whether name is a topic that Kafka takes and does not keep for itself.
func validTopic(name string) bool {
	return topicPattern.MatchString(name) && name != "." && name != ".." && !strings.HasPrefix(name, "__")
}

// cleanPath tells whether path is absolute and clean.
func cleanPath(path string) bool { return filepath.IsAbs(path) && filepath.Clean(path) == path }

func control(r rune) bool { return r < 0x20 || r == 0x7f }

// asciiLower gives host with each ASCII upper-case letter in lower case, and whether host is ASCII. A host name of a
// broker is compared in ASCII only: a Unicode case fold would match a host such as one with the Kelvin sign K (U+212A)
// to the ASCII host kafka.
func asciiLower(host string) (string, bool) {
	b := []byte(host)
	for i, c := range b {
		switch {
		case c >= 0x80:
			return "", false
		case 'A' <= c && c <= 'Z':
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b), true
}

// readFile reads a regular file of at most limit bytes. It opens the file without blocking, so a FIFO cannot hold
// the caller, and refuses anything that is not a regular file.
func readFile(path string, limit int64) ([]byte, error) { return read(path, limit, false) }

// privateModes are the mode bits that a secret file must not have: an execute bit, write for its group, or any access
// for others, as the service runtime refuses a setting file (ST-SUP-RP01-LC32). 0400, 0440, 0600 and 0640 pass.
const privateModes = 0o137

// readPrivate reads a secret file as readFile does, and refuses a file whose mode has a bit of privateModes. It checks
// the mode of the file that it opened, so a link that changes after the check reads nothing else.
func readPrivate(path string, limit int64) ([]byte, error) { return read(path, limit, true) }

func read(path string, limit int64, private bool) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if private && info.Mode().Perm()&privateModes != 0 {
		return nil, errors.New("the file has an execute bit, its group may write it, or others may use it")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("the file is too large")
	}
	return data, nil
}

// readSecret reads the secret file of the setting name: one value of at most 16 KiB with no control character, and
// a final line end taken off, from a file with no execute bit that its group may not write and others may not use. Its error
// names the setting and holds no part of the file.
func readSecret(name, path string) (string, error) {
	data, err := readPrivate(path, maxValue)
	if err != nil {
		return "", settingError(name, "must name a readable regular file of at most 16 KiB, with no execute bit, that its group may not write and others may not use")
	}
	secret := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if secret == "" || !utf8.ValidString(secret) || strings.ContainsFunc(secret, control) {
		return "", settingError(name, "must name a file of one value with no control character")
	}
	return secret, nil
}

package kafkaoutbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/oauth"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/jsell-rh/stego/registry/runtime/outbox"
)

// DestinationName is the name of the Kafka destination on the outbox worker. A command or a worker stages its change
// events for it as effects whose name is the event type, idempotent and not must land (ST-SUP-RP01-LC30).
const DestinationName = "kafka"

// The headers of each record that the destination publishes (ST-SUP-RP01-LC30).
const (
	// HeaderEventID is the event ID: the identity of the outbox message, the same on every send of one message, so a
	// consumer drops a duplicate by it.
	HeaderEventID = "stego-event-id"
	// HeaderIdempotencyKey is the outgoing idempotency key of the outbox message (ST-SUP-RP01-LC10).
	HeaderIdempotencyKey = "stego-idempotency-key"
	// HeaderEventType is the event type: the kind of the outbox message.
	HeaderEventType = "stego-event-type"
	// HeaderTenant is the tenant of the outbox row, never read from the payload.
	HeaderTenant = "stego-tenant"
	// HeaderContentType is application/json: the payload of the message, as it is.
	HeaderContentType = "content-type"
)

// The bounds of one delivery.
const (
	// DialTimeout bounds each connection to a broker.
	DialTimeout = 2 * time.Second
	// PingTimeout bounds the check that a broker answers before the record is sent; it takes at most half of the
	// time the attempt has left.
	PingTimeout = 2 * time.Second
	// DeliveryTimeout bounds one record in the client when the attempt's context sets no earlier end.
	DeliveryTimeout = time.Minute
	// MaxReadBytes is the largest answer that the client reads from a broker. The client allocates the size that an
	// answer announces before it reads it, so the bound keeps a hostile broker from making it allocate more.
	MaxReadBytes = 16 << 20
	// UnknownTopicRetries is how often the client sends a record again to a topic that the broker does not know
	// before the attempt fails.
	UnknownTopicRetries = 2
)

// The bounds of the SCRAM iteration count that a broker may ask for: Kafka takes 4096 to 16384 for SCRAM-SHA-512.
const (
	minSCRAMIterations = 4096
	maxSCRAMIterations = 16384
	maxSCRAMChallenge  = 16 << 10
)

// ErrClosed is the error of a delivery after Close.
var ErrClosed = errors.New("kafkaoutbox: the destination is closed")

// The errors of a delivery, one for each class of failure. Each is fixed text: it holds no broker address, no
// credential and no detail of the client's error.
var (
	errUnavailable    = errors.New("kafkaoutbox: no broker answered, so the record was not sent")
	errTLSVerify      = errors.New("kafkaoutbox: the certificate of a broker did not verify")
	errSASL           = errors.New("kafkaoutbox: a broker refused the SASL authentication")
	errSCRAMBounds    = errors.New("kafkaoutbox: the SCRAM challenge of a broker is out of its bounds")
	errBrokerRefused  = errors.New("kafkaoutbox: a broker named an address that the settings do not allow")
	errUnknownOutcome = errors.New("kafkaoutbox: the outcome of the record is not known")
)

// The failure classes of the log event kafka.delivery_failed.
const (
	FailureTLSVerification = "tls-verification"
	FailureSASL            = "sasl-authentication"
	FailureSCRAMBounds     = "scram-bounds"
	FailureBrokerRefused   = "broker-not-allowed"
)

// failure gives the fixed error and the failure class of a client error, or errUnavailable and "" when the error is
// of no class that the destination tells apart.
func failure(err error) (error, string) {
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.Is(err, errBrokerRefused):
		return errBrokerRefused, FailureBrokerRefused
	case errors.Is(err, errSCRAMBounds):
		return errSCRAMBounds, FailureSCRAMBounds
	case errors.As(err, &verify), errors.As(err, &authority), errors.As(err, &host), errors.As(err, &invalid):
		return errTLSVerify, FailureTLSVerification
	case errors.Is(err, kerr.SaslAuthenticationFailed), errors.Is(err, errSASL):
		return errSASL, FailureSASL
	}
	return errUnavailable, ""
}

// Destination is the Kafka destination of the outbox worker. It holds one client, which it opens again after an
// attempt whose outcome is not known, so a new attempt never shares a producer whose state is not known.
type Destination struct {
	options []kgo.Opt
	topic   string
	logger  otellog.Logger

	mu      sync.Mutex
	session *session
	closed  bool
}

type session struct {
	client *kgo.Client
	once   sync.Once
}

func (s *session) close() { s.once.Do(s.client.Close) }

// New gives the Kafka destination of the settings. It checks them, loads the CA file and the client certificate, and
// refuses a file that does not load with an error that names its setting and holds no secret. It opens no connection:
// the first delivery does. The client verifies each broker's certificate against the system roots and the CA file,
// asks for the acknowledgement of all in-sync replicas, and produces idempotently. It dials every broker, a seed or one
// that a broker names in its metadata, through one dialer that holds it to the settings. logs gives the event
// kafka.delivery_failed of a failure of a named class; nil emits none.
func New(s Settings, logs otellog.LoggerProvider) (*Destination, error) {
	if err := s.Check(); err != nil {
		return nil, err
	}
	dialer := &brokerDialer{loopbackOnly: s.InsecureLoopback}
	if len(s.BrokerHosts) > 0 {
		dialer.hosts = map[string]bool{}
		for _, h := range s.BrokerHosts {
			dialer.hosts[strings.ToLower(h)] = true
		}
	}
	options := []kgo.Opt{
		kgo.SeedBrokers(append([]string(nil), s.Brokers...)...),
		kgo.ClientID("stego-outbox"),
		kgo.Dialer(dialer.dial),
		kgo.BrokerMaxReadBytes(MaxReadBytes),
		// The destination fetches nothing; the fetch bound only has to stay within the read bound.
		kgo.FetchMaxBytes(MaxReadBytes),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerLinger(0),
		kgo.RecordDeliveryTimeout(DeliveryTimeout),
		kgo.UnknownTopicRetries(UnknownTopicRetries),
		kgo.StopProducerOnDataLossDetected(),
		kgo.MaxBufferedRecords(outbox.MaxBatchSize * 4),
		// The key hash of the Java client, so one entity's events go to one partition whatever client reads them.
		kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)),
		kgo.DisableClientMetrics(),
	}
	if !s.InsecureLoopback {
		config, err := tlsConfig(s)
		if err != nil {
			return nil, err
		}
		dialer.tls = config
	}
	switch s.Mechanism {
	case MechanismSCRAMSHA512:
		user, file := s.Username, s.PasswordFile
		options = append(options, kgo.SASL(boundedSCRAM{scram.Sha512(func(context.Context) (scram.Auth, error) {
			password, err := readSecret(PasswordFileSetting, file)
			if err != nil {
				return scram.Auth{}, err
			}
			return scram.Auth{User: user, Pass: password}, nil
		})}))
	case MechanismOAuthBearer:
		file := s.TokenFile
		options = append(options, kgo.SASL(refusedOAuth{oauth.Oauth(func(context.Context) (oauth.Auth, error) {
			token, err := readSecret(TokenFileSetting, file)
			if err != nil {
				return oauth.Auth{}, err
			}
			return oauth.Auth{Token: token}, nil
		})}))
	}
	d := &Destination{options: options, topic: s.Topic}
	if logs != nil {
		d.logger = logs.Logger("github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox")
	}
	return d, nil
}

// brokerDialer dials each broker for the client: a seed, or a broker that a broker names in its metadata. With hosts,
// it refuses a host that the list does not hold. In the insecure loopback mode it resolves the host, refuses it unless
// every address is a loopback address, and dials that address, so no broker gets a plaintext connection elsewhere.
// Else it dials TLS with the host as the server name, so the broker's certificate is verified for that host.
type brokerDialer struct {
	tls          *tls.Config
	loopbackOnly bool
	hosts        map[string]bool
}

func (b *brokerDialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errBrokerRefused
	}
	if b.hosts != nil && !b.hosts[strings.ToLower(host)] {
		return nil, errBrokerRefused
	}
	target := address
	if b.loopbackOnly {
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errBrokerRefused
		}
		for _, ip := range ips {
			if !ip.IP.IsLoopback() {
				return nil, errBrokerRefused
			}
		}
		target = net.JoinHostPort(ips[0].IP.String(), port)
	}
	conn, err := (&net.Dialer{Timeout: DialTimeout}).DialContext(ctx, network, target)
	if err != nil || b.tls == nil {
		return conn, err
	}
	config := b.tls.Clone()
	config.ServerName = host
	client := tls.Client(conn, config)
	handshake, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	if err := client.HandshakeContext(handshake); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

// tlsConfig gives the verified TLS configuration of the settings: TLS 1.2 at least, the system roots and the CA file,
// and the client certificate when the settings name one.
func tlsConfig(s Settings) (*tls.Config, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if s.CAFile != "" {
		data, err := readFile(s.CAFile, maxPEM)
		if err != nil || !roots.AppendCertsFromPEM(data) {
			return nil, settingError(CAFileSetting, "must name a readable PEM file of certificate authorities")
		}
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	if s.ClientCertFile != "" {
		certificate, err := readFile(s.ClientCertFile, maxPEM)
		if err != nil {
			return nil, settingError(ClientCertFileSetting, "must name a readable PEM file")
		}
		key, err := readFile(s.ClientKeyFile, maxPEM)
		if err != nil {
			return nil, settingError(ClientKeyFileSetting, "must name a readable PEM file")
		}
		pair, err := tls.X509KeyPair(certificate, key)
		if err != nil {
			return nil, settingError(ClientCertFileSetting+" and "+ClientKeyFileSetting, "must name a PEM certificate and its private key")
		}
		config.Certificates = []tls.Certificate{pair}
	}
	return config, nil
}

// Registration returns the registration of the destination on the outbox worker: the destination kafka, idempotent
// by the event ID, not must land, so a message whose attempts are exhausted is parked as on every other destination,
// with Deliver as its handler.
func (d *Destination) Registration() outbox.Destination {
	return outbox.Destination{Name: DestinationName, Idempotent: true, Handler: d.Deliver}
}

// Deliver is the outbox handler of the Kafka destination (ST-SUP-RP01-LC30, ST-SUP-RP01-LC31). It publishes the
// message as one record to the declared topic, or to the topic of its event type, with Key of its tenant and its
// resource key as the record key and the event ID, the idempotency key, the event type and the tenant as headers, and
// returns nil only when the broker acknowledged the record. It first asks a broker for its metadata, and marks the
// attempt not sent when none answers, so the outbox waits out a backoff. A refusal by the broker is transient, or
// permanent for a record that the broker can never take; any other end, and the end of the attempt's deadline, is an
// outcome that is not known, after which the destination opens its client again.
func (d *Destination) Deliver(ctx context.Context, _ *sql.Tx, delivery outbox.Delivery) error {
	record, err := d.record(delivery)
	if err != nil {
		return outbox.PermanentRefusal(err)
	}
	s, err := d.acquire()
	if err != nil {
		return outbox.NotSent(err)
	}
	if err := ping(ctx, s.client); err != nil {
		// No record was sent. A broker that does not answer may hold a connection the client must not use again.
		d.invalidate(s)
		fixed, class := failure(err)
		d.failed(ctx, class)
		return outbox.NotSent(fixed)
	}
	result := make(chan error, 1)
	s.client.Produce(ctx, record, func(_ *kgo.Record, err error) { result <- err })
	select {
	case err = <-result:
	case <-ctx.Done():
		// The record may be at the broker: the idempotent producer cannot drop it until the broker answers, so the
		// client is closed, which fails it, and a new attempt starts with a new client.
		d.invalidate(s)
		if err = <-result; err == nil {
			return nil
		}
		return ctx.Err()
	}
	return d.outcome(s, err)
}

// outcome marks the error of a produce. A broker's refusal of the batch means the record is not in the log: it is
// transient, or permanent for a record the broker can never take. A refusal after the append, a request timeout and
// every other error leave the outcome not known.
func (d *Destination) outcome(s *session, err error) error {
	if err == nil {
		return nil
	}
	if refusal, ok := errors.AsType[*kerr.Error](err); ok {
		switch refusal {
		case kerr.NotEnoughReplicasAfterAppend, kerr.RequestTimedOut:
			d.invalidate(s)
			return errors.New("kafkaoutbox: the broker may hold the record: " + refusal.Message)
		case kerr.MessageTooLarge, kerr.RecordListTooLarge, kerr.InvalidRecord, kerr.CorruptMessage, kerr.InvalidTopicException:
			return outbox.PermanentRefusal(errors.New("kafkaoutbox: the broker can never take the record: " + refusal.Message))
		default:
			return outbox.TransientRefusal(errors.New("kafkaoutbox: the broker refused the record: " + refusal.Message))
		}
	}
	d.invalidate(s)
	if _, class := failure(err); class != "" {
		d.failed(context.Background(), class)
	}
	return errUnknownOutcome
}

// failed emits the event kafka.delivery_failed for a failure of a named class: the class, and nothing else.
func (d *Destination) failed(ctx context.Context, class string) {
	if d.logger == nil || class == "" {
		return
	}
	var record otellog.Record
	record.SetEventName("kafka.delivery_failed")
	record.SetSeverity(otellog.SeverityWarn)
	record.SetSeverityText("WARN")
	record.SetBody(attribute.StringValue("a Kafka delivery failed: " + class))
	record.AddAttributes(attribute.String("failure", class))
	d.logger.Emit(ctx, record)
}

// record builds the record of one outbox message. It refuses a message with no tenant, no resource key, a payload
// that is not one JSON value, or an event type that is not a topic when no topic is declared.
func (d *Destination) record(delivery outbox.Delivery) (*kgo.Record, error) {
	if delivery.Tenant.IsZero() {
		return nil, errors.New("kafkaoutbox: the message has no tenant")
	}
	if delivery.ResourceKey == "" {
		return nil, errors.New("kafkaoutbox: the message has no resource key")
	}
	if len(delivery.Payload) == 0 || !json.Valid(delivery.Payload) {
		return nil, errors.New("kafkaoutbox: the payload is not one JSON value")
	}
	topic := d.topic
	if topic == "" {
		if !validTopic(delivery.Kind) {
			return nil, errors.New("kafkaoutbox: the event type is not a Kafka topic name")
		}
		topic = delivery.Kind
	}
	return &kgo.Record{
		Topic: topic,
		Key:   []byte(Key(delivery.Tenant.ID(), delivery.ResourceKey)),
		Value: bytes.Clone(delivery.Payload),
		Headers: []kgo.RecordHeader{
			{Key: HeaderEventID, Value: []byte(delivery.ID.String())},
			{Key: HeaderIdempotencyKey, Value: []byte(delivery.OutgoingKey)},
			{Key: HeaderEventType, Value: []byte(delivery.Kind)},
			{Key: HeaderTenant, Value: []byte(delivery.Tenant.ID())},
			{Key: HeaderContentType, Value: []byte("application/json")},
		},
	}, nil
}

// KeySeparator separates the tenant from the resource key in a record key. A tenant ID holds no control character
// (ST-SUP-RP04-LC01), so the tenant of a key is the text before its first separator.
const KeySeparator = "\x1f"

// Key gives the record key of a message of tenantID with resourceKey: the tenant, KeySeparator and the resource key,
// so two tenants with one entity ID never share a key (ST-SUP-RP01-LC30).
func Key(tenantID, resourceKey string) string { return tenantID + KeySeparator + resourceKey }

// ping asks a broker for its metadata within PingTimeout and half of the time that ctx has left, so a broker that does
// not answer leaves the attempt time to end as not sent before its deadline.
func ping(ctx context.Context, client *kgo.Client) error {
	limit := PingTimeout
	if deadline, ok := ctx.Deadline(); ok {
		limit = min(limit, time.Until(deadline)/2)
	}
	check, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	return client.Ping(check)
}

func (d *Destination) acquire() (*session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, ErrClosed
	}
	if d.session == nil {
		client, err := kgo.NewClient(d.options...)
		if err != nil {
			return nil, errors.New("kafkaoutbox: the client configuration is not valid")
		}
		d.session = &session{client: client}
	}
	return d.session, nil
}

// invalidate closes the client of s, and the next delivery opens a new one.
func (d *Destination) invalidate(s *session) {
	d.mu.Lock()
	if d.session == s {
		d.session = nil
	}
	d.mu.Unlock()
	s.close()
}

// Close closes the client. A delivery after Close is not sent.
func (d *Destination) Close() {
	d.mu.Lock()
	d.closed = true
	s := d.session
	d.session = nil
	d.mu.Unlock()
	if s != nil {
		s.close()
	}
}

// boundedSCRAM refuses a server-first message of more than 16 KiB or with an iteration count outside 4096 to 16384
// before the SCRAM library computes the salted password, so a hostile broker cannot make the client spend unbounded
// work.
type boundedSCRAM struct{ sasl.Mechanism }

func (m boundedSCRAM) Authenticate(ctx context.Context, host string) (sasl.Session, []byte, error) {
	s, first, err := m.Mechanism.Authenticate(ctx, host)
	if err != nil {
		return nil, nil, err
	}
	return &boundedSession{Session: s}, first, nil
}

type boundedSession struct {
	sasl.Session
	checked bool
}

func (s *boundedSession) Challenge(challenge []byte) (bool, []byte, error) {
	if len(challenge) > maxSCRAMChallenge {
		return false, nil, errSCRAMBounds
	}
	if !s.checked {
		s.checked = true
		iterations := -1
		for _, field := range strings.Split(string(challenge), ",") {
			if v, ok := strings.CutPrefix(field, "i="); ok {
				n, err := strconv.Atoi(v)
				if iterations != -1 || err != nil {
					return false, nil, errSCRAMBounds
				}
				iterations = n
			}
		}
		if iterations < minSCRAMIterations || iterations > maxSCRAMIterations {
			return false, nil, errSCRAMBounds
		}
	}
	return s.Session.Challenge(challenge)
}

// refusedOAuth marks the refusal of an OAUTHBEARER token as errSASL. A broker that refuses a token answers with an
// error document in a challenge (RFC 7628), which the OAUTHBEARER library reports as unexpected data.
type refusedOAuth struct{ sasl.Mechanism }

func (m refusedOAuth) Authenticate(ctx context.Context, host string) (sasl.Session, []byte, error) {
	s, first, err := m.Mechanism.Authenticate(ctx, host)
	if err != nil {
		return nil, nil, err
	}
	return refusedSession{s}, first, nil
}

type refusedSession struct{ sasl.Session }

func (s refusedSession) Challenge(challenge []byte) (bool, []byte, error) {
	done, response, err := s.Session.Challenge(challenge)
	if err != nil && len(challenge) > 0 {
		return false, nil, errSASL
	}
	return done, response, err
}

package auditsink

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/jsell-rh/stego/registry/runtime/audit"
)

// The attributes of each log record that the HTTPS sink sends.
const (
	AttributeEntryID  = "stego.audit.entry_id"
	AttributeHash     = "stego.audit.hash"
	AttributePrevHash = "stego.audit.prev_hash"
	AttributeTenant   = "stego.audit.tenant"
	AttributeKind     = "stego.audit.entry_kind"
)

// ScopeName is the instrumentation scope of the log records of the HTTPS sink.
const ScopeName = "stego.audit.sink"

// SendTimeout is the longest time of one send when the context has no earlier deadline.
const SendTimeout = 30 * time.Second

// maxResponse is the most bytes of a response that the HTTPS sink reads.
const maxResponse = 64 << 10

// maxToken is the largest token file that the HTTPS sink reads.
const maxToken = 16 << 10

// HTTPSSink sends each entry as one OpenTelemetry log record over OTLP/HTTP, binary protobuf, to the configured URL.
// A send is accepted only on a 2xx answer that rejects no log record; every other outcome is an error, so the
// outbox keeps the entry and sends it again. It does not deduplicate: the destination deduplicates by the entry ID,
// and the hash chain shows a gap or an edit.
type HTTPSSink struct {
	url        string
	tokenFile  string
	authHeader string
	client     *http.Client
}

var _ audit.Sink = (*HTTPSSink)(nil)

// NewHTTPSSink gives the HTTPS sink of the settings. Its client verifies the server's certificate against the system
// roots and the CA file, sends the client certificate when the settings name one, follows no redirect and uses no
// proxy. A file that does not load is refused with an error that names its setting and holds no secret.
func NewHTTPSSink(s Settings) (*HTTPSSink, error) {
	if err := s.Check(); err != nil {
		return nil, err
	}
	if !s.HTTPS() {
		return nil, settingError(URLSetting, "must be set for the HTTPS sink")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if s.CAFile != "" {
		data, err := os.ReadFile(s.CAFile)
		if err != nil || !roots.AppendCertsFromPEM(data) {
			return nil, settingError(CAFileSetting, "must name a readable PEM file of certificate authorities")
		}
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	if s.ClientCertFile != "" {
		pair, err := tls.LoadX509KeyPair(s.ClientCertFile, s.ClientKeyFile)
		if err != nil {
			return nil, settingError(ClientCertFileSetting+" and "+ClientKeyFileSetting, "must name a readable PEM certificate and its key")
		}
		config.Certificates = []tls.Certificate{pair}
	}
	if s.TokenFile != "" {
		if _, err := readToken(s.TokenFile); err != nil {
			return nil, err
		}
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: config, ForceAttemptHTTP2: true,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: SendTimeout, MaxIdleConnsPerHost: 4}
	client := &http.Client{Transport: transport, Timeout: SendTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &HTTPSSink{url: s.URL, tokenFile: s.TokenFile, authHeader: s.AuthHeader, client: client}, nil
}

// Append sends the events as one request with one log record each.
func (s *HTTPSSink) Append(ctx context.Context, events []audit.Event) error {
	entries, err := eventEntries(events)
	if err != nil {
		return err
	}
	return s.send(ctx, entries)
}

// Anchor sends the anchor as one request with one log record.
func (s *HTTPSSink) Anchor(ctx context.Context, a audit.Anchor) error {
	entry, err := anchorEntry(a)
	if err != nil {
		return err
	}
	return s.send(ctx, []Entry{entry})
}

// Request gives the OTLP logs request of the entries: one resource, one scope, one log record per entry, with the
// canonical entry as the body and the entry's ID, hash, previous hash, tenant and kind as attributes.
func Request(entries []Entry, observed time.Time) *collogs.ExportLogsServiceRequest {
	records := make([]*logs.LogRecord, 0, len(entries))
	for _, e := range entries {
		attributes := []*common.KeyValue{text(AttributeEntryID, e.ID), text(AttributeHash, hex.EncodeToString(e.Hash)),
			text(AttributeTenant, e.Tenant), text(AttributeKind, e.Kind)}
		if len(e.PrevHash) > 0 {
			attributes = append(attributes, text(AttributePrevHash, hex.EncodeToString(e.PrevHash)))
		}
		records = append(records, &logs.LogRecord{
			ObservedTimeUnixNano: uint64(observed.UnixNano()),
			SeverityNumber:       logs.SeverityNumber_SEVERITY_NUMBER_INFO,
			SeverityText:         "INFO",
			Body:                 &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: string(e.Body)}},
			Attributes:           attributes,
		})
	}
	return &collogs.ExportLogsServiceRequest{ResourceLogs: []*logs.ResourceLogs{{
		ScopeLogs: []*logs.ScopeLogs{{Scope: &common.InstrumentationScope{Name: ScopeName}, LogRecords: records}},
	}}}
}

func text(key, value string) *common.KeyValue {
	return &common.KeyValue{Key: key, Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: value}}}
}

// errNotAccepted is the error of a send that the endpoint did not accept.
var errNotAccepted = errors.New("auditsink: the endpoint of " + URLSetting + " did not accept the entries")

// send posts the entries and checks the answer. The token file is read again for each send, so a rotated token
// needs no restart.
func (s *HTTPSSink) send(ctx context.Context, entries []Entry) error {
	body, err := proto.Marshal(Request(entries, time.Now()))
	if err != nil {
		return fmt.Errorf("auditsink: the OTLP request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return settingError(URLSetting, "does not make a request")
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	if s.tokenFile != "" {
		token, err := readToken(s.tokenFile)
		if err != nil {
			return err
		}
		if s.authHeader == "" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else {
			req.Header.Set(s.authHeader, token)
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		// The error of the client names the URL, which can hold a query; the fault alone goes on.
		var cause *url.Error
		if errors.As(err, &cause) {
			err = cause.Err
		}
		return fmt.Errorf("%w: %w", errNotAccepted, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("%w: the answer did not read: %w", errNotAccepted, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: it answered %d", errNotAccepted, resp.StatusCode)
	}
	if len(data) > maxResponse {
		return fmt.Errorf("%w: its answer is larger than %d bytes", errNotAccepted, maxResponse)
	}
	var answer collogs.ExportLogsServiceResponse
	if err := proto.Unmarshal(data, &answer); err != nil {
		return fmt.Errorf("%w: its answer is not an OTLP logs response", errNotAccepted)
	}
	if partial := answer.GetPartialSuccess(); partial != nil && partial.GetRejectedLogRecords() > 0 {
		return fmt.Errorf("%w: it rejected %d log records", errNotAccepted, partial.GetRejectedLogRecords())
	}
	return nil
}

// readToken reads the token file: its text without a final line break, which must not be empty or hold a control
// character. A refusal names the setting and holds no part of the token.
func readToken(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", settingError(TokenFileSetting, "must name a readable file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxToken+1))
	if err != nil || len(data) > maxToken {
		return "", settingError(TokenFileSetting, "must name a readable file of at most 16 KiB")
	}
	token := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if token == "" || strings.ContainsFunc(token, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", settingError(TokenFileSetting, "must hold one token with no control character")
	}
	return token, nil
}

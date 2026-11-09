package otel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// The export limits of ST-SUP-RP03-LC10.
const (
	// QueueSize is the most spans and log records each queue holds before it drops.
	QueueSize = 256
	// BatchSize is the most spans or log records one export sends.
	BatchSize = 32
	// ExportTimeout is the deadline of each export.
	ExportTimeout = 2 * time.Second
	// ShutdownTimeout is the time Close gives all providers together.
	ShutdownTimeout = 3 * time.Second
	// ResponseLimit is the largest export response the runtime reads.
	ResponseLimit = 64 << 10
	// RequestLimit is the largest export request the runtime sends.
	RequestLimit = 1 << 20
	// MetricSeriesLimit is the most series of each instrument, the SDK overflow series included.
	MetricSeriesLimit = 128
)

const (
	batchDelay          = 200 * time.Millisecond
	certificateLimit    = 64 << 10
	tokenLimit          = 4096
	tokenSetting        = "STEGO_OTEL_TOKEN_FILE"
	defaultSampleRatio  = 0.1
	defaultExportPeriod = 10 * time.Second
)

// Runtime owns the private tracer, meter and logger providers of one service process and their one export
// connection. It never sets a global OpenTelemetry provider. Without a collector endpoint it starts no provider
// and opens no connection (ST-SUP-RP03-LC01).
type Runtime struct {
	service    string
	instance   string
	resource   *resource.Resource
	connection *grpc.ClientConn
	provider   *sdktrace.TracerProvider
	spans      *spanQueue
	tracer     trace.Tracer
	rpcTracer  trace.Tracer
	meter      *sdkmetric.MeterProvider
	logs       *sdklog.LoggerProvider
	signals    requestSignals
	records    sync.Map // spanKey to *spanRecord, for each recording span of the runtime that has not ended

	once             sync.Once
	closed           atomic.Bool
	traceFailures    atomic.Uint64
	metricFailures   atomic.Uint64
	logFailures      atomic.Uint64
	shutdownFailures atomic.Uint64
	shutdownErr      error
}

// settings are the telemetry settings of the process environment, checked (ST-SUP-RP03-LC09).
type settings struct {
	service     string
	endpoint    string
	origin      *url.URL
	certificate string
	token       string
	ratio       float64
	interval    time.Duration
	metrics     bool
	logs        bool
}

// New reads the telemetry settings from the process environment and starts the runtime of the service with the
// declared name. OTEL_SERVICE_NAME, when set, takes the place of the declared name. Without
// OTEL_EXPORTER_OTLP_ENDPOINT it returns a runtime with no provider and no connection. With it, it opens one
// verified TLS gRPC connection and does not wait for the collector (ST-SUP-RP03-LC02).
func New(service string) (*Runtime, error) {
	values, err := readSettings(service, os.Environ())
	if err != nil {
		return nil, err
	}
	instance, err := newInstanceID(rand.Reader)
	if err != nil {
		return nil, err
	}
	r := &Runtime{service: values.service, instance: instance, resource: serviceResource(values.service, instance)}
	if values.endpoint == "" {
		return r, nil
	}
	connection, err := dial(values)
	if err != nil {
		return nil, err
	}
	r.connection = connection
	traces, err := otlptracegrpc.New(context.Background(), otlptracegrpc.WithGRPCConn(connection), otlptracegrpc.WithHeaders(map[string]string{}), otlptracegrpc.WithTimeout(ExportTimeout), otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: false}), otlptracegrpc.WithMaxRequestSize(RequestLimit))
	if err != nil {
		_ = connection.Close()
		return nil, errors.New("cannot create the trace exporter")
	}
	r.spans = newSpanQueue(&safeSpanExporter{SpanExporter: traces, failures: &r.traceFailures})
	r.startTraces(values.ratio, r.spans)
	var reader sdkmetric.Reader
	if values.metrics {
		exporter, err := otlpmetricgrpc.New(context.Background(), otlpmetricgrpc.WithGRPCConn(connection), otlpmetricgrpc.WithHeaders(map[string]string{}), otlpmetricgrpc.WithTimeout(ExportTimeout), otlpmetricgrpc.WithRetry(otlpmetricgrpc.RetryConfig{Enabled: false}), otlpmetricgrpc.WithMaxRequestSize(RequestLimit), otlpmetricgrpc.WithTemporalitySelector(cumulative))
		if err != nil {
			r.Close()
			return nil, errors.New("cannot create the metric exporter")
		}
		reader = sdkmetric.NewPeriodicReader(&safeMetricExporter{Exporter: exporter, failures: &r.metricFailures}, sdkmetric.WithInterval(values.interval), sdkmetric.WithTimeout(ExportTimeout))
	}
	var processor sdklog.Processor
	if values.logs {
		exporter, err := otlploggrpc.New(context.Background(), otlploggrpc.WithGRPCConn(connection), otlploggrpc.WithHeaders(map[string]string{}), otlploggrpc.WithTimeout(ExportTimeout), otlploggrpc.WithRetry(otlploggrpc.RetryConfig{Enabled: false}), otlploggrpc.WithMaxRequestSize(RequestLimit))
		if err != nil {
			r.Close()
			return nil, errors.New("cannot create the log exporter")
		}
		processor = sdklog.NewBatchProcessor(&safeLogExporter{Exporter: exporter, failures: &r.logFailures}, sdklog.WithMaxQueueSize(QueueSize), sdklog.WithExportMaxBatchSize(BatchSize), sdklog.WithExportInterval(batchDelay), sdklog.WithExportTimeout(ExportTimeout))
	}
	if err := r.startSignals(reader, processor); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// dial opens the one export connection: TLS 1.3 verified against the trust roots, no proxy, no retry, no service
// config, and the request and response limits of ST-SUP-RP03-LC02 and ST-SUP-RP03-LC10. It does not connect yet.
func dial(values settings) (*grpc.ClientConn, error) {
	roots, err := trustRoots(values.certificate)
	if err != nil {
		return nil, err
	}
	options := []grpc.DialOption{
		grpc.WithNoProxy(),
		grpc.WithDisableRetry(),
		grpc.WithDisableServiceConfig(),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: values.origin.Hostname()})),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(ResponseLimit), grpc.MaxCallSendMsgSize(RequestLimit)),
	}
	if values.token != "" {
		token, err := readToken(values.token)
		clear(token)
		if err != nil {
			return nil, err
		}
		options = append(options, grpc.WithPerRPCCredentials(tokenCredential{file: values.token}))
	}
	connection, err := grpc.NewClient("passthrough:///"+values.origin.Host, options...)
	if err != nil {
		return nil, errors.New("cannot create the telemetry export connection")
	}
	return connection, nil
}

// startTraces starts the private tracer provider with the local ratio sampler and the span processor. The
// sampler reads only the trace ID, so a remote sampled flag never changes the decision (ST-SUP-RP03-LC08). The SDK
// span keeps no event and no link, and the processor gets each span of the runtime only as the runtime itself set
// it (ownSpans), so code that holds the span in its context cannot add a name, an attribute, a status message, an
// event or a span of its own (ST-SUP-RP03-LC05).
func (r *Runtime) startTraces(ratio float64, processor sdktrace.SpanProcessor) {
	r.provider = sdktrace.NewTracerProvider(
		sdktrace.WithResource(r.resource),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(ratio)),
		sdktrace.WithRawSpanLimits(sdktrace.SpanLimits{AttributeValueLengthLimit: 256, AttributeCountLimit: 8}),
		sdktrace.WithSpanProcessor(&ownSpans{next: processor, records: &r.records}),
	)
	r.tracer = r.provider.Tracer("stego/http")
	r.rpcTracer = r.provider.Tracer("stego/grpc")
}

// Shutdown shuts the providers down together, then closes the export connection. The whole shutdown ends by the
// earlier of the deadline of ctx and ShutdownTimeout, so a caller can share one shutdown limit with other work
// (ST-SUP-RC17-OC29). It is safe to call more than once: a later call returns the result of the first.
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.once.Do(func() {
		r.closed.Store(true)
		ctx, cancel := context.WithTimeout(ctx, ShutdownTimeout)
		defer cancel()
		var group sync.WaitGroup
		var failed atomic.Bool
		shut := func(shutdown func(context.Context) error) {
			group.Add(1)
			go func() {
				defer group.Done()
				if err := shutdown(ctx); err != nil {
					r.shutdownFailures.Add(1)
					failed.Store(true)
				}
			}()
		}
		if r.provider != nil {
			shut(r.provider.Shutdown)
		}
		if r.meter != nil {
			shut(r.meter.Shutdown)
		}
		if r.logs != nil {
			shut(r.logs.Shutdown)
		}
		group.Wait()
		if r.connection != nil {
			_ = r.connection.Close()
		}
		if failed.Load() {
			r.shutdownErr = errors.New("telemetry shutdown did not complete")
		}
	})
	return r.shutdownErr
}

// Close is Shutdown with no deadline but ShutdownTimeout.
func (r *Runtime) Close() { _ = r.Shutdown(context.Background()) }

// TracerProvider is the private tracer provider, or nil when no collector was set at start.
func (r *Runtime) TracerProvider() trace.TracerProvider {
	if r.provider == nil {
		return nil
	}
	return r.provider
}

// MeterProvider is the private meter provider, or nil when no collector was set at start or metrics are off.
func (r *Runtime) MeterProvider() metric.MeterProvider {
	if r.meter == nil {
		return nil
	}
	return r.meter
}

// LoggerProvider is the private logger provider, or nil when no collector was set at start or logs are off.
func (r *Runtime) LoggerProvider() otellog.LoggerProvider {
	if r.logs == nil {
		return nil
	}
	return r.logs
}

// CollectorSet tells whether a collector endpoint was set when the runtime started.
func (r *Runtime) CollectorSet() bool { return r.connection != nil }

// Service is the service name the runtime reports.
func (r *Runtime) Service() string { return r.service }

// Instance is the random identity of this process that the runtime reports.
func (r *Runtime) Instance() string { return r.instance }

// ExportFailures counts the trace export batches that failed. It keeps no remote error text.
func (r *Runtime) ExportFailures() uint64 { return r.traceFailures.Load() }

// MetricExportFailures counts the metric exports that failed.
func (r *Runtime) MetricExportFailures() uint64 { return r.metricFailures.Load() }

// LogExportFailures counts the log export batches that failed.
func (r *Runtime) LogExportFailures() uint64 { return r.logFailures.Load() }

// ShutdownFailures counts the providers that did not shut down inside the shutdown limit.
func (r *Runtime) ShutdownFailures() uint64 { return r.shutdownFailures.Load() }

// DroppedSpans counts the sampled spans that were not exported: dropped by the full queue, ended after shutdown,
// in a failed export or left when shutdown ended.
func (r *Runtime) DroppedSpans() uint64 {
	if r.spans == nil {
		return 0
	}
	return r.spans.dropped.Load()
}

// continueTrace returns the context with the remote parent of a single valid traceparent value. Any other set of
// values, an invalid one or more than one, leaves the context as it is, so the span starts a new trace
// (ST-SUP-RP03-LC08). It reads no trace state and no baggage.
func continueTrace(ctx context.Context, values []string) context.Context {
	if len(values) != 1 || len(values[0]) != 55 {
		return ctx
	}
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": values[0]})
}

// readSettings checks the telemetry settings in environ against the fixed shape of ST-SUP-RP03-LC09. The
// declared name is the service name unless OTEL_SERVICE_NAME is set. An OTEL_ setting the runtime does not know is
// refused, so no resource attribute or exporter can come in from the environment.
func readSettings(declared string, environ []string) (settings, error) {
	var values settings
	known := map[string]bool{"OTEL_EXPORTER_OTLP_ENDPOINT": true, "OTEL_EXPORTER_OTLP_PROTOCOL": true, "OTEL_EXPORTER_OTLP_CERTIFICATE": true, "OTEL_SERVICE_NAME": true, "OTEL_TRACES_SAMPLER_ARG": true, "OTEL_METRICS_EXPORTER": true, "OTEL_LOGS_EXPORTER": true, "OTEL_METRIC_EXPORT_INTERVAL": true, tokenSetting: true}
	set := map[string]string{}
	for _, entry := range environ {
		name, value, _ := strings.Cut(entry, "=")
		if value == "" || (!strings.HasPrefix(name, "OTEL_") && name != tokenSetting) {
			continue
		}
		if !known[name] || len(value) > 4096 {
			return values, errors.New("unsupported telemetry setting " + safeName(name))
		}
		set[name] = value
	}
	service := set["OTEL_SERVICE_NAME"]
	if service == "" {
		service = declared
	}
	if service == "" || len(service) > 128 || strings.Trim(service, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") != "" {
		return values, errors.New("invalid telemetry service name")
	}
	values.service = service
	values.endpoint = set["OTEL_EXPORTER_OTLP_ENDPOINT"]
	values.certificate = set["OTEL_EXPORTER_OTLP_CERTIFICATE"]
	values.token = set[tokenSetting]
	if values.endpoint != "" {
		origin, err := collectorOrigin(values.endpoint)
		if err != nil {
			return values, err
		}
		values.origin = origin
	} else if values.certificate != "" || values.token != "" {
		return values, errors.New("telemetry credentials require OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if protocol := set["OTEL_EXPORTER_OTLP_PROTOCOL"]; protocol != "" && protocol != "grpc" {
		return values, errors.New("OTEL_EXPORTER_OTLP_PROTOCOL must be grpc")
	}
	values.ratio = defaultSampleRatio
	if value := set["OTEL_TRACES_SAMPLER_ARG"]; value != "" {
		ratio, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 || ratio > 1 {
			return values, errors.New("OTEL_TRACES_SAMPLER_ARG must be a ratio from 0 to 1")
		}
		values.ratio = ratio
	}
	values.interval = defaultExportPeriod
	if value := set["OTEL_METRIC_EXPORT_INTERVAL"]; value != "" {
		ms, err := strconv.Atoi(value)
		if err != nil || ms < 1000 || ms > 60000 || strconv.Itoa(ms) != value {
			return values, errors.New("OTEL_METRIC_EXPORT_INTERVAL must be 1000 to 60000 milliseconds")
		}
		values.interval = time.Duration(ms) * time.Millisecond
	}
	for _, name := range []string{"OTEL_METRICS_EXPORTER", "OTEL_LOGS_EXPORTER"} {
		if value := set[name]; value != "" && value != "otlp" && value != "none" {
			return values, errors.New(name + " must be otlp or none")
		}
	}
	values.metrics = set["OTEL_METRICS_EXPORTER"] != "none"
	values.logs = set["OTEL_LOGS_EXPORTER"] != "none"
	return values, nil
}

// safeName gives the name of a setting for an error only when it is a plain setting name, so an error never
// echoes other environment text.
func safeName(name string) string {
	if len(name) > 64 || strings.Trim(name, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") != "" {
		return "name"
	}
	return name
}

// collectorOrigin accepts only a plain HTTPS origin: a host and an optional port, no user, path, query or fragment.
// With no port, the origin is the host at port 443.
func collectorOrigin(endpoint string) (*url.URL, error) {
	origin, err := url.Parse(endpoint)
	if len(endpoint) > 2048 || err != nil || origin.Scheme != "https" || origin.Hostname() == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.Path != "" || origin.Opaque != "" {
		return nil, errors.New("OTEL_EXPORTER_OTLP_ENDPOINT must be an HTTPS origin")
	}
	if strings.HasSuffix(origin.Host, ":") || strings.Contains(origin.Host, "%") {
		return nil, errors.New("OTEL_EXPORTER_OTLP_ENDPOINT has an invalid address")
	}
	if value := origin.Port(); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != value {
			return nil, errors.New("OTEL_EXPORTER_OTLP_ENDPOINT has an invalid port")
		}
	} else {
		// The connection dials the host as it is, so an origin with no port gets the HTTPS port.
		origin.Host = net.JoinHostPort(origin.Hostname(), "443")
	}
	return origin, nil
}

// readFile reads a telemetry file only when it is an absolute path to a regular file inside the limit. It opens
// without blocking, so a link to a device or a pipe cannot hold startup. A private file must not be readable or
// writable by others, nor writable or executable by its group.
func readFile(name string, limit int64, private bool) ([]byte, error) {
	invalid := errors.New("telemetry file is unavailable or invalid")
	if name == "" || len(name) > 4096 || !filepath.IsAbs(name) {
		return nil, invalid
	}
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, invalid
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit || (private && info.Mode().Perm()&0137 != 0) {
		return nil, invalid
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		clear(data)
		return nil, invalid
	}
	return data, nil
}

// trustRoots is the system root pool when name is empty, and otherwise the certificates of the PEM file it names
// and nothing else (ST-SUP-RP03-LC09).
func trustRoots(name string) (*x509.CertPool, error) {
	if name == "" {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			return nil, errors.New("cannot read the system trust roots")
		}
		return roots, nil
	}
	data, err := readFile(name, certificateLimit, false)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	return certificatePool(data)
}

// certificatePool reads PEM certificate blocks only, at most 128, with no other block, header or text.
func certificatePool(data []byte) (*x509.CertPool, error) {
	invalid := errors.New("OTEL_EXPORTER_OTLP_CERTIFICATE must hold certificate PEM only")
	roots := x509.NewCertPool()
	count := 0
	for rest := bytes.TrimSpace(data); len(rest) > 0; {
		if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, invalid
		}
		block, tail := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, invalid
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, invalid
		}
		roots.AddCert(certificate)
		count++
		if count > 128 {
			return nil, invalid
		}
		rest = bytes.TrimSpace(tail)
	}
	if count == 0 {
		return nil, invalid
	}
	return roots, nil
}

// readToken reads the private bearer token file. The caller clears the token after use.
func readToken(name string) ([]byte, error) {
	data, err := readFile(name, tokenLimit, true)
	if err != nil {
		return nil, err
	}
	value := bytes.TrimSpace(data)
	if len(value) < 16 || bytes.IndexFunc(value, func(r rune) bool {
		return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~+/=", r)
	}) >= 0 {
		clear(data)
		return nil, errors.New("invalid telemetry token")
	}
	token := bytes.Clone(value)
	clear(data)
	return token, nil
}

// tokenCredential reads the token file again at each export, so a rotated token takes effect with no restart.
type tokenCredential struct{ file string }

func (tokenCredential) RequireTransportSecurity() bool { return true }

func (c tokenCredential) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	token, err := readToken(c.file)
	if err != nil {
		return nil, err
	}
	defer clear(token)
	return map[string]string{"authorization": "Bearer " + string(token)}, nil
}

// newInstanceID is a random version 4 UUID that names this process.
func newInstanceID(source io.Reader) (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(source, value[:]); err != nil {
		return "", errors.New("cannot create the telemetry instance identity")
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	text := hex.EncodeToString(value[:])
	return text[0:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:], nil
}

// serviceResource holds the service name and the instance identity, and nothing from the environment.
func serviceResource(service, instance string) *resource.Resource {
	return resource.NewSchemaless(attribute.String("service.name", service), attribute.String("service.instance.id", instance))
}

func cumulative(sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.CumulativeTemporality
}

// spanQueue is the bounded trace queue of ST-SUP-RP03-LC03 and ST-SUP-RP03-LC10. A span that finds the queue full
// is dropped and counted; the request never waits. One goroutine sends batches of at most BatchSize, each export
// bounded by ExportTimeout and by the queue's base context, which shutdown cancels when its own deadline ends.
// While an export is in flight the queue still takes spans up to QueueSize. Every sampled span that ends is either
// exported or counted in dropped: a full queue, a span that ends after shutdown, a failed export and the spans left
// when shutdown ends all count.
type spanQueue struct {
	exporter sdktrace.SpanExporter
	queue    chan sdktrace.ReadOnlySpan
	flushes  chan chan struct{}
	stop     chan struct{}
	done     chan struct{}
	base     context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once
	mu       sync.RWMutex
	closed   bool
	dropped  atomic.Uint64
}

func newSpanQueue(exporter sdktrace.SpanExporter) *spanQueue {
	base, cancel := context.WithCancel(context.Background())
	q := &spanQueue{exporter: exporter, queue: make(chan sdktrace.ReadOnlySpan, QueueSize), flushes: make(chan chan struct{}), stop: make(chan struct{}), done: make(chan struct{}), base: base, cancel: cancel}
	go q.run()
	return q
}

func (q *spanQueue) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

// OnEnd queues the span, or counts it dropped. Shutdown sets closed under the write lock, so no span enters the
// queue after run has started its last count.
func (q *spanQueue) OnEnd(span sdktrace.ReadOnlySpan) {
	if !span.SpanContext().IsSampled() {
		return
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		q.dropped.Add(1)
		return
	}
	select {
	case q.queue <- span:
	default:
		q.dropped.Add(1)
	}
}

func (q *spanQueue) run() {
	defer close(q.done)
	batch := make([]sdktrace.ReadOnlySpan, 0, BatchSize)
	timer := time.NewTimer(batchDelay)
	defer timer.Stop()
	send := func() {
		if len(batch) == 0 {
			return
		}
		export, cancel := context.WithTimeout(q.base, ExportTimeout)
		if err := q.exporter.ExportSpans(export, batch); err != nil {
			q.dropped.Add(uint64(len(batch)))
		}
		cancel()
		clear(batch)
		batch = batch[:0]
	}
	drain := func() {
		for q.base.Err() == nil {
			select {
			case span := <-q.queue:
				batch = append(batch, span)
				if len(batch) == BatchSize {
					send()
				}
			default:
				send()
				return
			}
		}
	}
	stopped := func() bool {
		select {
		case <-q.stop:
			drain()
			q.dropped.Add(uint64(len(batch) + len(q.queue)))
			return true
		default:
			return false
		}
	}
	for !stopped() {
		select {
		case span := <-q.queue:
			batch = append(batch, span)
			if len(batch) == BatchSize {
				send()
				timer.Reset(batchDelay)
			}
		case <-timer.C:
			send()
			timer.Reset(batchDelay)
		case reply := <-q.flushes:
			drain()
			close(reply)
		case <-q.stop:
		}
	}
}

func (q *spanQueue) ForceFlush(ctx context.Context) error {
	reply := make(chan struct{})
	select {
	case q.flushes <- reply:
	case <-q.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown stops the queue: run sends what the queue holds until ctx ends, then the base context is cancelled, the
// export in flight returns, and run counts what is left as dropped. The exporter shuts down only after run ended.
func (q *spanQueue) Shutdown(ctx context.Context) error {
	q.stopOnce.Do(func() {
		q.mu.Lock()
		q.closed = true
		q.mu.Unlock()
		close(q.stop)
	})
	var err error
	select {
	case <-q.done:
	case <-ctx.Done():
		err = ctx.Err()
		q.cancel()
		<-q.done
	}
	q.cancel()
	if shutdownErr := q.exporter.Shutdown(ctx); err == nil {
		err = shutdownErr
	}
	return err
}

// safeSpanExporter counts each failed batch and answers with a fixed error, so no collector text reaches a log.
type safeSpanExporter struct {
	sdktrace.SpanExporter
	failures *atomic.Uint64
}

func (e *safeSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if err := e.SpanExporter.ExportSpans(ctx, spans); err != nil {
		e.failures.Add(1)
		return errors.New("trace export failed")
	}
	return nil
}

type safeMetricExporter struct {
	sdkmetric.Exporter
	failures *atomic.Uint64
}

func (e *safeMetricExporter) Export(ctx context.Context, data *metricdata.ResourceMetrics) error {
	if err := e.Exporter.Export(ctx, data); err != nil {
		e.failures.Add(1)
		return errors.New("metric export failed")
	}
	return nil
}

type safeLogExporter struct {
	sdklog.Exporter
	failures *atomic.Uint64
}

func (e *safeLogExporter) Export(ctx context.Context, records []sdklog.Record) error {
	if err := e.Exporter.Export(ctx, records); err != nil {
		e.failures.Add(1)
		return errors.New("log export failed")
	}
	return nil
}

// SpanEventLimit is the most probe events that one span keeps (ST-SUP-RP12-LC04): the emissions one call holds.
const SpanEventLimit = 64

// spanKey names one span.
type spanKey struct {
	trace trace.TraceID
	span  trace.SpanID
}

func keyOf(sc trace.SpanContext) spanKey { return spanKey{trace: sc.TraceID(), span: sc.SpanID()} }

// spanRecord is what the runtime itself set on one of its spans: the name, the attributes, the failure mark and
// the probe events. The span is exported with these only.
type spanRecord struct {
	mu            sync.Mutex
	name          string
	attrs         []attribute.KeyValue
	failed        bool
	events        []sdktrace.Event
	droppedEvents int
	ended         bool
}

func (s *spanRecord) setName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
}

// set sets attributes, each key once, the last value kept.
func (s *spanRecord) set(attrs ...attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, kv := range attrs {
		if i := slices.IndexFunc(s.attrs, func(old attribute.KeyValue) bool { return old.Key == kv.Key }); i >= 0 {
			s.attrs[i] = kv
		} else if len(s.attrs) < 8 {
			s.attrs = append(s.attrs, kv)
		}
	}
}

func (s *spanRecord) fail() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = true
}

// startSpan starts a span of the runtime and keeps its record while it records.
func (r *Runtime) startSpan(ctx context.Context, tracer trace.Tracer, name string, kind trace.SpanKind, start time.Time, attrs ...attribute.KeyValue) (context.Context, trace.Span, *spanRecord) {
	ctx, span := tracer.Start(ctx, name, trace.WithSpanKind(kind), trace.WithAttributes(attrs...), trace.WithTimestamp(start))
	record := &spanRecord{name: name}
	record.set(attrs...)
	if span.IsRecording() {
		r.records.Store(keyOf(span.SpanContext()), record)
	}
	return ctx, span, record
}

// SpanEvent adds one event of the probe runtime to a recording span of this runtime. The name starts with domain.
// and the attributes are the declared fields of the observation and stego.outcome, at most nine. It tells whether
// the event was dropped: the span is full or has ended, or the event is not well formed. A span that is not one of
// this runtime's recording spans takes no event, and that is no drop.
func (r *Runtime) SpanEvent(sc trace.SpanContext, name string, at time.Time, attrs ...attribute.KeyValue) (dropped bool) {
	value, ok := r.records.Load(keyOf(sc))
	if !ok {
		return false
	}
	if !strings.HasPrefix(name, "domain.") || len(name) > 128 || len(attrs) > 9 {
		return true
	}
	kept := make([]attribute.KeyValue, len(attrs))
	for i, kv := range attrs {
		if kv.Value.Type() == attribute.STRING && len(kv.Value.AsString()) > 256 {
			kv = attribute.String(string(kv.Key), kv.Value.AsString()[:256])
		}
		kept[i] = kv
	}
	record := value.(*spanRecord)
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.ended || len(record.events) >= SpanEventLimit {
		record.droppedEvents++
		return true
	}
	record.events = append(record.events, sdktrace.Event{Name: name, Attributes: kept, Time: at})
	return false
}

// ownSpans gives the next processor each span of the runtime as its record holds it, and no other span: a span
// that code started with the provider of a span in its context is not exported.
type ownSpans struct {
	next    sdktrace.SpanProcessor
	records *sync.Map
}

func (p *ownSpans) OnStart(ctx context.Context, span sdktrace.ReadWriteSpan) {
	p.next.OnStart(ctx, span)
}

func (p *ownSpans) OnEnd(span sdktrace.ReadOnlySpan) {
	value, ok := p.records.LoadAndDelete(keyOf(span.SpanContext()))
	if !ok {
		return
	}
	record := value.(*spanRecord)
	record.mu.Lock()
	record.ended = true
	own := &ownSpan{ReadOnlySpan: span, name: record.name, attrs: slices.Clone(record.attrs), events: slices.Clone(record.events), droppedEvents: record.droppedEvents}
	if record.failed {
		own.status = sdktrace.Status{Code: codes.Error}
	}
	record.mu.Unlock()
	p.next.OnEnd(own)
}

func (p *ownSpans) Shutdown(ctx context.Context) error   { return p.next.Shutdown(ctx) }
func (p *ownSpans) ForceFlush(ctx context.Context) error { return p.next.ForceFlush(ctx) }

// ownSpan is an ended span with the name, the attributes, the status and the events of its record, no status
// message and no link.
type ownSpan struct {
	sdktrace.ReadOnlySpan
	name          string
	attrs         []attribute.KeyValue
	status        sdktrace.Status
	events        []sdktrace.Event
	droppedEvents int
}

func (s *ownSpan) Name() string                     { return s.name }
func (s *ownSpan) Attributes() []attribute.KeyValue { return s.attrs }
func (s *ownSpan) Status() sdktrace.Status          { return s.status }
func (s *ownSpan) Events() []sdktrace.Event         { return s.events }
func (s *ownSpan) Links() []sdktrace.Link           { return nil }
func (s *ownSpan) DroppedAttributes() int           { return 0 }
func (s *ownSpan) DroppedEvents() int               { return s.droppedEvents }
func (s *ownSpan) DroppedLinks() int                { return 0 }

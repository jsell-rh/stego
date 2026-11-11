package otel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// refusals is an authentication and a capacity admission step that refuse a request and note the time they did.
type refusals struct {
	mu   sync.Mutex
	when []time.Time
}

func (f *refusals) refuse(w http.ResponseWriter, code int) {
	f.mu.Lock()
	f.when = append(f.when, time.Now())
	f.mu.Unlock()
	w.WriteHeader(code)
}

func (f *refusals) last() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.when[len(f.when)-1]
}

func TestServerSpanBounds(t *testing.T) {
	t.Run("ST-SUP-RP03-LC07-T01 The server span starts before authentication and capacity admission", func(t *testing.T) {
		h := newHarness(t, 1)
		f := &refusals{}
		chain := h.runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				f.refuse(w, http.StatusUnauthorized)
				return
			}
			f.refuse(w, http.StatusServiceUnavailable)
		}))
		for _, tc := range []struct {
			name   string
			token  string
			status string
		}{{"authentication", "", "401"}, {"capacity", "Bearer token-value", "503"}} {
			request := httptest.NewRequest("GET", "/records", nil)
			if tc.token != "" {
				request.Header.Set("Authorization", tc.token)
			}
			chain.ServeHTTP(httptest.NewRecorder(), request)
			span := h.lastSpan(t)
			if got, _ := spanAttribute(span, "http.response.status_code"); got != tc.status || !span.StartTime().Before(f.last()) {
				t.Fatalf("HTTP %s refusal: status %s, span start %v, refusal %v", tc.name, got, span.StartTime(), f.last())
			}
		}
		for _, tc := range []struct {
			name string
			code grpccodes.Code
		}{{"authentication", grpccodes.Unauthenticated}, {"capacity", grpccodes.ResourceExhausted}} {
			_, finish := h.runtime.TraceRPC(context.Background(), "/sample.v1.Records/Get")
			refused := time.Now()
			finish(status.Error(tc.code, "refused"))
			span := h.lastSpan(t)
			if got, _ := spanAttribute(span, "rpc.response.status_code"); got != statusNames[tc.code] || !span.StartTime().Before(refused) {
				t.Fatalf("RPC %s refusal: status %s", tc.name, got)
			}
		}
	})
	t.Run("ST-SUP-RP03-LC07-T02 The server span ends only after the final status is known", func(t *testing.T) {
		h := newHarness(t, 1)
		var lastByte time.Time
		h.runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			for i := range 3 {
				fmt.Fprintf(w, "part %d\n", i)
				http.NewResponseController(w).Flush()
				time.Sleep(10 * time.Millisecond)
			}
			lastByte = time.Now()
		})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/stream", nil))
		span := h.lastSpan(t)
		if got, _ := spanAttribute(span, "http.response.status_code"); got != "202" || span.EndTime().Before(lastByte) {
			t.Fatal("the HTTP span ended before the last byte or lost the status")
		}
		_, finish := h.runtime.TraceRPC(context.Background(), "/sample.v1.Records/Watch")
		time.Sleep(10 * time.Millisecond)
		before := time.Now()
		finish(status.Error(grpccodes.NotFound, "gone"))
		span = h.lastSpan(t)
		if got, _ := spanAttribute(span, "rpc.response.status_code"); got != "NOT_FOUND" || span.EndTime().Before(before) {
			t.Fatal("the RPC span ended before the final status")
		}
	})
	t.Run("ST-SUP-RP03-LC07-T03 The span wrapper does not change the observed call", func(t *testing.T) {
		startCollector(t, false)
		on, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		defer on.Close()
		clearEnvironment(t)
		off, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		if on.provider == nil || off.provider != nil {
			t.Fatal("the two runs do not differ in telemetry")
		}
		with, without := observedCalls(t, on), observedCalls(t, off)
		if !slices.Equal(with, without) {
			t.Fatalf("telemetry changed the call:\n on: %q\noff: %q", with, without)
		}
	})
	t.Run("ST-SUP-RP03-LC07-T04 Health and discovery routes stay outside the request wrappers", func(t *testing.T) {
		h := newHarness(t, 1)
		// The assembler's shape: a top router holds the public routes and mounts the wrapped application on /.
		public := []string{"/livez", "/readyz", "/v1/openapi", "/v1/openapi.html", "/v1"}
		app := http.NewServeMux()
		app.Handle("/", &countingHandler{})
		top := http.NewServeMux()
		served := &countingHandler{}
		for _, path := range public {
			top.Handle("GET "+path, served)
		}
		top.Handle("/", h.runtime.Handler(h.runtime.Route(app)))
		for _, path := range public {
			top.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
		}
		if served.calls.Load() != int32(len(public)) {
			t.Fatal("a public route was not served by its own handler")
		}
		if len(h.spans.Ended()) != 0 || len(h.spans.Started()) != 0 || len(h.logs.all()) != 0 || histogramCount(h.collect(t), "http.server.request.duration") != 0 {
			t.Fatal("a public route has a span, a metric sample or a completion log")
		}
		// An encoded slash is not a public route: it reaches the wrapped application and is observed.
		top.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1%2Fopenapi", nil))
		if served.calls.Load() != int32(len(public)) || len(h.spans.Ended()) != 1 || len(h.logs.all()) != 1 || histogramCount(h.collect(t), "http.server.request.duration") != 1 {
			t.Fatal("an encoded public path went past the wrappers unobserved")
		}
	})
}

// observedCalls serves a fixed set of calls through the runtime's wrappers and a real server, and writes down what
// a client and the handlers see: statuses, headers, bodies, deadlines and the order of a shutdown.
func observedCalls(t *testing.T, r *Runtime) []string {
	t.Helper()
	var mu sync.Mutex
	var notes []string
	note := func(format string, args ...any) {
		mu.Lock()
		notes = append(notes, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	deadline := time.Now().Add(time.Hour).Truncate(time.Second)
	permits := make(chan struct{}, 1)
	holding, release := make(chan struct{}), make(chan struct{})
	draining, shutdownBegun := make(chan struct{}), make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/records/{id}", func(w http.ResponseWriter, req *http.Request) {
		got, ok := req.Context().Deadline()
		note("records deadline kept %v", ok && got.Equal(deadline))
		w.Header().Set("X-Record", req.PathValue("id"))
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "record body")
	})
	mux.HandleFunc("GET /v1/hold", func(w http.ResponseWriter, _ *http.Request) {
		close(holding)
		<-release
		io.WriteString(w, "held")
	})
	mux.HandleFunc("GET /v1/stream", func(w http.ResponseWriter, req *http.Request) {
		controller := http.NewResponseController(w)
		note("write deadline set %v", controller.SetWriteDeadline(time.Now().Add(time.Second)) == nil)
		note("read deadline set %v", controller.SetReadDeadline(time.Now().Add(time.Second)) == nil)
		ctx, cancel := context.WithTimeout(req.Context(), 50*time.Millisecond)
		defer cancel()
		io.WriteString(w, "tick\n")
		controller.Flush()
		<-ctx.Done()
		note("stream expired %v", ctx.Err())
		io.WriteString(w, "expired\n")
	})
	mux.HandleFunc("GET /v1/drain", func(w http.ResponseWriter, _ *http.Request) {
		note("handler started")
		close(draining)
		<-shutdownBegun
		time.Sleep(50 * time.Millisecond)
		io.WriteString(w, "drained")
		note("handler ended")
	})
	capacity := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			select {
			case permits <- struct{}{}:
				defer func() { <-permits }()
				next.ServeHTTP(w, req)
			default:
				w.Header().Set("Retry-After", "1")
				http.Error(w, "capacity exceeded", http.StatusServiceUnavailable)
			}
		})
	}
	authenticate := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer token-value" {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, req.Clone(req.Context()))
		})
	}
	withDeadline := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx, cancel := context.WithDeadline(req.Context(), deadline)
			defer cancel()
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	server := httptest.NewServer(withDeadline(r.Handler(authenticate(capacity(r.Route(mux))))))
	defer server.Close()
	client := server.Client()
	// describe makes one call and gives what the client sees; call notes it at once.
	describe := func(path, token string) string {
		request, _ := http.NewRequest("GET", server.URL+path, nil)
		if token != "" {
			request.Header.Set("Authorization", token)
		}
		response, err := client.Do(request)
		if err != nil {
			return fmt.Sprintf("%s error %v", path, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		var headers []string
		for name, values := range response.Header {
			if name != "Date" {
				headers = append(headers, name+"="+strings.Join(values, ","))
			}
		}
		sort.Strings(headers)
		return fmt.Sprintf("%s status %d headers %v body %q", path, response.StatusCode, headers, body)
	}
	call := func(path, token string) { note("%s", describe(path, token)) }
	token := "Bearer token-value"
	call("/v1/records/private-id", token)
	call("/v1/records/private-id", "")
	held := make(chan struct{})
	go func() { call("/v1/hold", token); close(held) }()
	<-holding
	call("/v1/records/private-id", token)
	close(release)
	<-held
	call("/v1/stream", token)
	// The client's view of the drained call is noted after the shutdown returned: the client reads the answer and
	// the server's Shutdown returns at about the same time, so only the order of the server's own steps is fixed.
	drained := make(chan struct{})
	var drainedCall string
	go func() { drainedCall = describe("/v1/drain", token); close(drained) }()
	<-draining
	note("shutdown began")
	shutdownDone := make(chan error)
	go func() { shutdownDone <- server.Config.Shutdown(context.Background()) }()
	time.Sleep(10 * time.Millisecond)
	close(shutdownBegun)
	note("shutdown returned %v", <-shutdownDone)
	<-drained
	note("%s", drainedCall)
	ctx, cancel := context.WithDeadline(metadata.NewIncomingContext(context.Background(), metadata.Pairs("traceparent", parent)), deadline)
	defer cancel()
	got, finish := r.TraceRPC(ctx, "/sample.v1.Records/Get")
	rpcDeadline, ok := got.Deadline()
	note("rpc deadline kept %v, metadata kept %v", ok && rpcDeadline.Equal(deadline), metadata.ValueFromIncomingContext(got, "traceparent") != nil)
	finish(nil)
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(notes)
}

// histogramCount is the number of samples of the named histogram in the collected metrics.
func histogramCount(data metricdata.ResourceMetrics, name string) uint64 {
	var count uint64
	for _, point := range histogramPoints(data, name) {
		count += point.Count
	}
	return count
}

func histogramPoints(data metricdata.ResourceMetrics, name string) []metricdata.HistogramDataPoint[float64] {
	var points []metricdata.HistogramDataPoint[float64]
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if histogram, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Name == name {
				points = append(points, histogram.DataPoints...)
			}
		}
	}
	return points
}

func findMetric(data metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func attributeKeys(set attribute.Set) []string {
	var keys []string
	for _, kv := range set.ToSlice() {
		keys = append(keys, string(kv.Key))
	}
	sort.Strings(keys)
	return keys
}

func TestRequestMetrics(t *testing.T) {
	t.Run("ST-SUP-RP03-LC11-T01 The request duration histograms have fixed names, unit, boundaries and temporality", func(t *testing.T) {
		sink := startCollector(t, false)
		r, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		r.Handler(&countingHandler{}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records", nil))
		_, finish := r.TraceRPC(context.Background(), "/sample.v1.Records/Get")
		finish(nil)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := r.meter.ForceFlush(ctx); err != nil {
			t.Fatal(err)
		}
		request := <-sink.metrics
		metrics := metricMap(request)
		want := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300, 1800}
		for _, name := range []string{"http.server.request.duration", "rpc.server.call.duration"} {
			m, ok := metrics[name]
			if !ok || m.GetHistogram() == nil {
				t.Fatalf("%s is not a histogram", name)
			}
			histogram := m.GetHistogram()
			if m.Unit != "s" || histogram.AggregationTemporality.String() != "AGGREGATION_TEMPORALITY_CUMULATIVE" || len(histogram.DataPoints) != 1 || !slices.Equal(histogram.DataPoints[0].ExplicitBounds, want) {
				t.Fatalf("%s has the wrong unit, temporality or boundaries: %v", name, m)
			}
		}
	})
	t.Run("ST-SUP-RP03-LC11-T02 The duration labels are only method, route, status and error class", func(t *testing.T) {
		h := newHarness(t, 1)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /records/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		mux.HandleFunc("GET /widgets/{id}/parts/{part}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
		handler := h.runtime.Handler(h.runtime.Route(mux))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records/private-1", nil))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/widgets/private-2/parts/private-3", nil))
		points := histogramPoints(h.collect(t), "http.server.request.duration")
		if len(points) != 2 {
			t.Fatal("expected one series for each route, got", len(points))
		}
		for _, point := range points {
			keys := attributeKeys(point.Attributes)
			route, _ := point.Attributes.Value("http.route")
			code, _ := point.Attributes.Value("http.response.status_code")
			switch route.AsString() {
			case "/records/{id}":
				if !slices.Equal(keys, []string{"http.request.method", "http.response.status_code", "http.route"}) || code.AsInt64() != 200 {
					t.Fatal("unexpected labels of a success", keys)
				}
			case "/widgets/{id}/parts/{part}":
				class, _ := point.Attributes.Value("error.type")
				if !slices.Equal(keys, []string{"error.type", "http.request.method", "http.response.status_code", "http.route"}) || !fixedErrorClass(class.AsString()) {
					t.Fatal("unexpected labels of a failure", keys, class.AsString())
				}
			default:
				t.Fatal("the route label is not a registered pattern", route.AsString())
			}
		}
	})
	t.Run("ST-SUP-RP03-LC11-T03 The active request counters carry no label", func(t *testing.T) {
		h := newHarness(t, 1)
		entered, release := make(chan struct{}), make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.runtime.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				close(entered)
				<-release
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records", nil))
		}()
		<-entered
		_, finish := h.runtime.TraceRPC(context.Background(), "/sample.v1.Records/Watch")
		data := h.collect(t)
		close(release)
		<-done
		finish(nil)
		for _, name := range []string{"http.server.active_requests", "rpc.server.active_requests"} {
			m, ok := findMetric(data, name)
			sum, isSum := m.Data.(metricdata.Sum[int64])
			if !ok || !isSum || sum.IsMonotonic || len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 1 || sum.DataPoints[0].Attributes.Len() != 0 {
				t.Fatalf("%s is not a label-free up-down counter at 1: %+v", name, m)
			}
		}
	})
	t.Run("ST-SUP-RP03-LC11-T04 No metric label holds a trace or span ID", func(t *testing.T) {
		h := newHarness(t, 1)
		var span trace.SpanContext
		mux := http.NewServeMux()
		mux.HandleFunc("GET /records/{id}", func(w http.ResponseWriter, r *http.Request) {
			span = trace.SpanContextFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		})
		request := httptest.NewRequest("GET", "/records/1", nil)
		request.Header.Set("traceparent", parent)
		h.runtime.Handler(mux).ServeHTTP(httptest.NewRecorder(), request)
		if !span.IsSampled() {
			t.Fatal("the request was not sampled")
		}
		for _, value := range metricLabelValues(h.collect(t)) {
			if strings.Contains(value, span.TraceID().String()) || strings.Contains(value, span.SpanID().String()) {
				t.Fatal("a metric label holds a trace or span ID", value)
			}
		}
	})
}

// fixedErrorClass tells whether the class is one of the fixed values: panic or a server status from 500 to 599.
func fixedErrorClass(class string) bool {
	if class == "panic" {
		return true
	}
	code, err := strconv.Atoi(class)
	return err == nil && code >= 500 && code <= 599
}

// metricLabelValues lists every attribute value of every data point; exemplars are not labels.
func metricLabelValues(data metricdata.ResourceMetrics) []string {
	var values []string
	add := func(set attribute.Set) {
		for _, kv := range set.ToSlice() {
			values = append(values, kv.Value.Emit())
		}
	}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, point := range d.DataPoints {
					add(point.Attributes)
				}
			case metricdata.Sum[int64]:
				for _, point := range d.DataPoints {
					add(point.Attributes)
				}
			}
		}
	}
	return values
}

// logField is the string form of the named field of a log record, if it has one.
func logField(record interface {
	WalkAttributes(func(attribute.KeyValue) bool)
}, key string) (string, bool) {
	var value string
	found := false
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		if string(kv.Key) == key {
			value, found = kv.Value.Emit(), true
			return false
		}
		return true
	})
	return value, found
}

func TestRoutePattern(t *testing.T) {
	routeOf := func(t *testing.T, h *harness) (span, metric, logged string) {
		t.Helper()
		if ended := h.spans.Ended(); len(ended) > 0 {
			span, _ = spanAttribute(ended[len(ended)-1], "http.route")
		}
		if points := histogramPoints(h.collect(t), "http.server.request.duration"); len(points) == 1 {
			value, _ := points[0].Attributes.Value("http.route")
			metric = value.AsString()
		}
		if records := h.logs.all(); len(records) == 1 {
			logged, _ = logField(&records[0], "http.route")
		}
		return span, metric, logged
	}
	t.Run("ST-SUP-RP03-LC14-T01 The route pattern survives a copied request", func(t *testing.T) {
		h := newHarness(t, 1)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /widgets/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		inner := h.runtime.Route(mux)
		authenticate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			inner.ServeHTTP(w, r.WithContext(ctx))
		})
		h.runtime.Handler(authenticate).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/widgets/private-widget", nil))
		span, metric, logged := routeOf(t, h)
		if span != "/widgets/{id}" || metric != span || logged != span || h.lastSpan(t).Name() != "GET /widgets/{id}" {
			t.Fatalf("the copied request lost the route: span %q, metric %q, log %q", span, metric, logged)
		}
	})
	t.Run("ST-SUP-RP03-LC14-T02 The route pattern is kept at sampling ratio zero", func(t *testing.T) {
		h := newHarness(t, 0)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /widgets/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		request := httptest.NewRequest("GET", "/widgets/private-widget", nil)
		request.Header.Set("traceparent", parent)
		h.runtime.Handler(h.runtime.Route(mux)).ServeHTTP(httptest.NewRecorder(), request)
		if len(h.spans.Ended()) != 0 {
			t.Fatal("a span was recorded at ratio zero")
		}
		_, metric, logged := routeOf(t, h)
		if metric != "/widgets/{id}" || logged != metric {
			t.Fatalf("ratio zero lost the route: metric %q, log %q", metric, logged)
		}
	})
	t.Run("ST-SUP-RP03-LC14-T03 Metrics and logs do not take their fields from recorded span attributes", func(t *testing.T) {
		h := newHarness(t, 1)
		marker := "marker-from-span"
		mux := http.NewServeMux()
		mux.HandleFunc("GET /widgets/{id}", func(w http.ResponseWriter, r *http.Request) {
			trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("http.route", marker), attribute.String("http.request.method", marker), attribute.String("error.type", marker))
			w.WriteHeader(http.StatusNoContent)
		})
		h.runtime.Handler(h.runtime.Route(mux)).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/widgets/1", nil))
		for _, value := range metricLabelValues(h.collect(t)) {
			if value == marker {
				t.Fatal("a metric label came from a span attribute")
			}
		}
		for _, record := range h.logs.all() {
			record.WalkAttributes(func(kv attribute.KeyValue) bool {
				if kv.Value.Emit() == marker {
					t.Fatal("a log field came from a span attribute")
				}
				return true
			})
		}
	})
}

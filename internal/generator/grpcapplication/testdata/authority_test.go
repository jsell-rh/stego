package sample

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// checkMissingAuthority verifies the transport fix for GO-2026-6443.
// TestRuntime then checks that valid requests still succeed.
func checkMissingAuthority(t *testing.T, address string, roots *x509.CertPool) {
	t.Helper()
	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", address,
		&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	framer := http2.NewFramer(connection, connection)
	framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	framer.MaxHeaderListSize = 8192
	if err := framer.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: "POST"},
		{Name: ":scheme", Value: "https"},
		{Name: ":path", Value: "/sample.v1.Records/Echo"},
		{Name: "content-type", Value: "application/grpc"},
		{Name: "te", Value: "trailers"},
	} {
		if err := encoder.WriteField(field); err != nil {
			t.Fatal(err)
		}
	}
	if err := framer.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndHeaders: true, EndStream: true}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatal("missing authority did not receive a rejection", err)
		}
		if settings, ok := frame.(*http2.SettingsFrame); ok && !settings.IsAck() {
			if err := framer.WriteSettingsAck(); err != nil {
				t.Fatal(err)
			}
		}
		if headers, ok := frame.(*http2.MetaHeadersFrame); ok && headers.StreamID == 1 {
			values := make(map[string]string)
			for _, field := range headers.Fields {
				values[field.Name] = field.Value
			}
			if headers.Truncated || !headers.StreamEnded() || values[":status"] != "400" || values["grpc-status"] != "13" {
				t.Fatalf("unexpected missing-authority response: %v", values)
			}
			return
		}
	}
	t.Fatal("missing authority exceeded the response frame limit")
}

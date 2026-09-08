package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener
}

func waitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
		return nil
	}
}

func TestHTTPShutdownDrainsActiveRequest(t *testing.T) {
	listener := testListener(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := stegoHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
			w.Write([]byte("complete"))
		case <-r.Context().Done():
			t.Error("shutdown canceled a request before the drain deadline")
		}
	}))
	result := make(chan error, 1)
	go func() { result <- stegoServeHTTP(ctx, listener, server, time.Second) }()
	request := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		response, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			defer response.Body.Close()
			body, readErr := io.ReadAll(response.Body)
			err = readErr
			if string(body) != "complete" {
				err = errors.New("request did not complete during drain")
			}
		}
		request <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case err := <-result:
		t.Fatalf("shutdown returned before the active request: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := waitResult(t, request); err != nil {
		t.Fatal(err)
	}
	if err := waitResult(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPShutdownClosesRequestsAfterDeadline(t *testing.T) {
	listener := testListener(t)
	entered, exited := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := stegoHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(exited)
	}))
	result := make(chan error, 1)
	go func() { result <- stegoServeHTTP(ctx, listener, server, 20*time.Millisecond) }()
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	if err := waitResult(t, result); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost drain deadline error: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("force-close did not cancel the request")
	}
}

func TestHTTPNetworkLimits(t *testing.T) {
	for _, kind := range []string{"headers", "slow-header", "slow-body", "slow-write", "idle"} {
		t.Run(kind, func(t *testing.T) {
			listener := testListener(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := stegoHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "slow-write" {
					time.Sleep(100 * time.Millisecond)
				}
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					http.Error(w, "request body timeout", http.StatusRequestTimeout)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			if server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.WriteTimeout <= 0 || server.IdleTimeout <= 0 || server.MaxHeaderBytes != 32<<10 {
				t.Fatal("server network limits are missing")
			}
			server.ReadHeaderTimeout = 30 * time.Millisecond
			server.ReadTimeout = 60 * time.Millisecond
			server.WriteTimeout = 80 * time.Millisecond
			server.IdleTimeout = 30 * time.Millisecond
			result := make(chan error, 1)
			go func() { result <- stegoServeHTTP(ctx, listener, server, time.Second) }()
			conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(time.Second))
			request := "GET / HTTP/1.1\r\nHost: test\r\n"
			switch kind {
			case "headers":
				request += "X-Large: " + strings.Repeat("a", 64<<10) + "\r\n\r\n"
			case "slow-header":
				request += "X-Incomplete: "
			case "slow-body":
				request = "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 100\r\n\r\na"
			default:
				request += "\r\n"
			}
			if _, err := io.WriteString(conn, request); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if kind == "slow-header" || kind == "slow-write" {
				if err == nil {
					response.Body.Close()
					t.Fatal("server accepted a request after its network deadline")
				}
				if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
					t.Fatal("client deadline expired before the server closed the connection")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				want := http.StatusRequestHeaderFieldsTooLarge
				if kind == "slow-body" {
					want = http.StatusRequestTimeout
				} else if kind == "idle" {
					want = http.StatusNoContent
				}
				if response.StatusCode != want {
					t.Fatalf("status = %d, want %d", response.StatusCode, want)
				}
				if kind == "idle" {
					var one [1]byte
					if _, err := conn.Read(one[:]); !errors.Is(err, io.EOF) {
						t.Fatalf("idle connection was not closed: %v", err)
					}
				}
			}
			cancel()
			if err := waitResult(t, result); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHTTPListenerFailureReturnsError(t *testing.T) {
	listener := testListener(t)
	listener.Close()
	err := stegoServeHTTP(context.Background(), listener, stegoHTTPServer(http.NotFoundHandler()), time.Second)
	if err == nil {
		t.Fatal("closed listener returned success")
	}
}

func TestHTTPCanceledStartClosesListener(t *testing.T) {
	listener := testListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := stegoServeHTTP(ctx, listener, stegoHTTPServer(http.NotFoundHandler()), time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled startup returned %v", err)
	}
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("canceled startup did not close the listener: %v", err)
	}
}

func TestHTTPSignalChild(t *testing.T) {
	if os.Getenv("STEGO_HTTP_SIGNAL_TEST") != "1" {
		return
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSignalStopsService(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support this POSIX signal test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHTTPSignalChild$")
	cmd.Env = append(os.Environ(), "STEGO_HTTP_SIGNAL_TEST=1", "PORT=0")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	address := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			const prefix = "starting server on "
			if index := strings.Index(line, prefix); index >= 0 {
				address <- line[index+len(prefix):]
			}
		}
	}()
	var addr string
	select {
	case addr = <-address:
	case <-time.After(3 * time.Second):
		t.Fatal("child service did not open a listener")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://127.0.0.1:" + port)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- cmd.Wait() }()
	if err := waitResult(t, result); err != nil {
		t.Fatalf("SIGTERM did not stop the service successfully: %v", err)
	}
}

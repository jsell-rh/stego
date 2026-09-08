package compiler

var httpLifecycleImports = []string{"context", "errors", "net", "net/http", "os/signal", "syscall", "time"}

const httpLifecycleSource = `

// stegoHTTPServer sets limits for the generated request-response API.
func stegoHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler: handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 32 << 10,
	}
}

// stegoServeHTTP owns the listener. Shutdown first drains active requests.
// After the deadline, it closes remaining connections and returns an error.
func stegoServeHTTP(ctx context.Context, listener net.Listener, server *http.Server, drainTimeout time.Duration) error {
	defer listener.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	select {
	case err := <-result:
		return errors.Join(stegoHTTPError(err), server.Close())
	case <-ctx.Done():
		drain, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		err := server.Shutdown(drain)
		if err != nil {
			err = errors.Join(err, server.Close())
		}
		return errors.Join(err, stegoHTTPError(<-result))
	}
}

func stegoHTTPError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
`

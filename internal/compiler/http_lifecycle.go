package compiler

import "fmt"

var httpLifecycleImports = []string{"context", "errors", "net", "net/http", "sync", "sync/atomic", "os/signal", "syscall", "time"}

const httpLifecycleSource = `

// stegoHTTPServer sets limits for the generated request-response API.
func stegoHTTPServer(handler http.Handler) *http.Server {
 return stegoHTTPServerWithErrorLog(handler, nil)
}

func stegoHTTPServerWithErrorLog(handler http.Handler, errorLog *log.Logger) *http.Server {
 if errorLog==nil {errorLog=stegoNewHTTPErrorLog(os.Stderr)}
	return &http.Server{
		Handler: handler,
        ErrorLog: errorLog,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 32 << 10,
	}
}

// stegoHTTPRequests tracks the full handler chain, including telemetry and
// handlers that hijack a connection. net/http does not drain hijacked handlers.
type stegoHTTPRequests struct {
 next http.Handler
 mu sync.Mutex
 active int
 stopped bool
 drained chan struct{}
}
func(s *stegoHTTPRequests)ServeHTTP(w http.ResponseWriter,r *http.Request){
 s.mu.Lock()
 if s.stopped{s.mu.Unlock();http.Error(w,http.StatusText(http.StatusServiceUnavailable),http.StatusServiceUnavailable);return}
 s.active++;s.mu.Unlock()
 defer func(){s.mu.Lock();s.active--;if s.stopped&&s.active==0{close(s.drained)};s.mu.Unlock()}()
 s.next.ServeHTTP(w,r)
}
func(s *stegoHTTPRequests)drain(ctx context.Context)error{
 s.mu.Lock();if !s.stopped{s.stopped=true;if s.active==0{close(s.drained)}};s.mu.Unlock()
 select{case <-s.drained:return nil;default:}
 select{case <-s.drained:return nil;case <-ctx.Done():return ctx.Err()}
}

// stegoServeHTTP owns the listener. Shutdown drains the complete handler chain.
// Components must cancel and close the connections that they hijack. A handler
// that remains after the drain deadline causes an error, not a successful stop.
func stegoServeHTTP(ctx context.Context, listener net.Listener, server *http.Server, drainTimeout time.Duration) error {
 defer stegoCloseHTTPDiagnostics(server)
 defer listener.Close()
 if err:=ctx.Err();err!=nil{return err}
 next:=server.Handler;if next==nil{next=http.DefaultServeMux}
 requests:=&stegoHTTPRequests{next:next,drained:make(chan struct{})}
 server.Handler=requests
 result:=make(chan error,1)
 go func(){result<-server.Serve(listener)}()
 select{
 case err:=<-result:
  closed:=server.Close()
  drain,cancel:=context.WithTimeout(context.Background(),drainTimeout);defer cancel()
  return errors.Join(stegoHTTPError(err),closed,requests.drain(drain))
 case <-ctx.Done():
  drain,cancel:=context.WithTimeout(context.Background(),drainTimeout);defer cancel()
  err:=server.Shutdown(drain)
  if err!=nil{err=errors.Join(err,server.Close())}
  return errors.Join(err,requests.drain(drain),stegoHTTPError(<-result))
 }
}

func stegoHTTPError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
` + httpDiagnosticsSource

func httpServerExpression(input AssemblerInput, renames map[int][]constructorRename, handler string) string {
	for i, component := range input.Wirings {
		if component.Wiring == nil || component.Wiring.HTTPErrorLogger == nil {
			continue
		}
		index := *component.Wiring.HTTPErrorLogger
		name := rawConstructorVarName(component.Wiring.Constructors[index])
		for _, rename := range renames[i] {
			if rename.ConstructorIndex == index {
				name = rename.FinalVar
				break
			}
		}
		return fmt.Sprintf("stegoHTTPServerWithErrorLog(%s, %s.HTTPErrorLog())", handler, name)
	}
	return fmt.Sprintf("stegoHTTPServer(%s)", handler)
}

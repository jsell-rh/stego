package compiler

// Services without a telemetry provider still need safe, bounded diagnostics.
const httpDiagnosticsSource = `

type stegoHTTPDiagnostics struct {
 mu sync.Mutex
 output io.Writer
 queue chan time.Time
 done chan struct{}
 closed bool
 dropped, failures atomic.Uint64
}

func stegoNewHTTPErrorLog(output io.Writer)*log.Logger {
 return log.New(&stegoHTTPDiagnostics{output:output,done:make(chan struct{})},"",0)
}

// Write does not retain raw server output. It queues only the event time.
// The worker starts on the first diagnostic. Callers never wait for output.
func(d *stegoHTTPDiagnostics)Write(data []byte)(int,error){
 d.mu.Lock();defer d.mu.Unlock()
 if d.closed {d.dropped.Add(1);return len(data),nil}
 if d.queue==nil {
  d.queue=make(chan time.Time,256)
  go func(){
   defer close(d.done)
   encoder:=json.NewEncoder(d.output)
   for timestamp:=range d.queue {
    record:=struct{
     Time time.Time ` + "`json:\"timestamp\"`" + `
     Severity string ` + "`json:\"severity\"`" + `
     Event string ` + "`json:\"event.name\"`" + `
     Message string ` + "`json:\"message\"`" + `
    }{timestamp,"ERROR","http.server.diagnostic","HTTP server reported a diagnostic"}
    if err:=encoder.Encode(record);err!=nil{d.failures.Add(1)}
   }
  }()
 }
 select {case d.queue<-time.Now().UTC():default:d.dropped.Add(1)}
 return len(data),nil
}

func(d *stegoHTTPDiagnostics)close(){
 d.mu.Lock()
 if !d.closed {
  d.closed=true
  if d.queue!=nil {close(d.queue)} else {close(d.done)}
 }
 d.mu.Unlock()
 timer:=time.NewTimer(time.Second);defer timer.Stop()
 select{case <-d.done:case <-timer.C:}
}

// The server owns only the fallback logger. Component loggers keep their owner.
func stegoCloseHTTPDiagnostics(server *http.Server){
 if server.ErrorLog==nil{return}
 if diagnostics,ok:=server.ErrorLog.Writer().(*stegoHTTPDiagnostics);ok{diagnostics.close()}
}
`

package compiler

// The process boundary must not format error causes. Driver and component
// errors can contain credentials, query values, paths, or provider responses.
const serviceFailureSource = `

type stegoServiceFailure struct {
 stage string
 cause error
 tasks []string
}

func (e *stegoServiceFailure) Error() string { return "service failed at " + e.stage }
func (e *stegoServiceFailure) Unwrap() error { return e.cause }

// stegoReportFailure writes one fixed record before process exit. The stage
// comes from generated code. The cause is never formatted or serialized.
// A blocked output can retain one worker until the process exits.
func stegoReportFailure(output io.Writer, err error) {
 stage := "service.run"
 var tasks []string
 if failure, ok := err.(*stegoServiceFailure); ok { stage = failure.stage; tasks = failure.tasks }
 record := struct {
  Timestamp string ` + "`json:\"timestamp\"`" + `
  Severity string ` + "`json:\"severity\"`" + `
  Event string ` + "`json:\"event.name\"`" + `
  Message string ` + "`json:\"message\"`" + `
  Stage string ` + "`json:\"stage\"`" + `
  Tasks []string ` + "`json:\"tasks,omitempty\"`" + `
 }{time.Now().UTC().Format(time.RFC3339Nano), "ERROR", "service.failed", "Service failed", stage, tasks}
 done := make(chan struct{})
 go func() {
  defer close(done)
  json.NewEncoder(output).Encode(record)
 }()
 timer := time.NewTimer(time.Second)
 defer timer.Stop()
 select {
 case <-done:
 case <-timer.C:
 }
}
`

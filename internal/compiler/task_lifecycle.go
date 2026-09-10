package compiler

import (
	"bytes"
	"fmt"
)

var taskLifecycleImports = []string{"context", "errors", "sort", "os/signal", "syscall"}

func hasBackgroundTasks(input AssemblerInput) bool {
	for _, cw := range input.Wirings {
		if cw.Wiring != nil && len(cw.Wiring.BackgroundTasks) != 0 {
			return true
		}
	}
	return false
}

func writeSignalContext(buf *bytes.Buffer) {
	buf.WriteString("\tctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)\n\tdefer stop()\n")
}

func writeBackgroundStart(buf *bytes.Buffer, input AssemblerInput, renames map[int][]constructorRename, httpHandler string) {
	buf.WriteString("\tstegoStage = \"service.run\"\n")
	buf.WriteString("\treturn stegoRunTasks(ctx, []stegoTask{\n")
	if httpHandler != "" {
		fmt.Fprintf(buf, "{name: \"http\", run: func(ctx context.Context) error { return stegoServeHTTP(ctx, listener, %s, 10*time.Second) }},\n", httpServerExpression(input, renames, httpHandler))
	}
	for i, cw := range input.Wirings {
		if cw.Wiring == nil {
			continue
		}
		for _, index := range cw.Wiring.BackgroundTasks {
			name := rawConstructorVarName(cw.Wiring.Constructors[index])
			for _, rename := range renames[i] {
				if rename.ConstructorIndex == index {
					name = rename.FinalVar
					break
				}
			}
			fmt.Fprintf(buf, "{name: %q, run: %s.Run},\n", fmt.Sprintf("%s[%d]", cw.Name, index), name)
		}
	}
	buf.WriteString("\t})\n")
}

const taskLifecycleSource = `

type stegoTaskFailure struct { name string; cause error; aborted bool }
func (e *stegoTaskFailure) Error() string { return "task " + e.name + " failed" }
func (e *stegoTaskFailure) Unwrap() error { return e.cause }

// stegoTaskNames reads only the direct task failures made by stegoRunTasks.
// It does not inspect or format component error causes.
func stegoTaskNames(err error) []string { return stegoTaskFailureNames(err, false) }
func stegoAbortedTaskNames(err error) []string { return stegoTaskFailureNames(err, true) }
func stegoTaskFailureNames(err error, abortedOnly bool) []string {
 joined, ok := err.(interface { Unwrap() []error })
 if !ok { return nil }
 var names []string
 for _, cause := range joined.Unwrap() {
  if failure, ok := cause.(*stegoTaskFailure); ok && (!abortedOnly || failure.aborted) { names = append(names, failure.name) }
 }
 sort.Strings(names)
 return names
}


// stegoTask runs until cancellation. It must return after cancellation.
type stegoTask struct {
 name string
 run func(context.Context) error
}

// stegoRunTasks cancels all tasks when one task fails. It waits for every task
// before return, so deferred resource cleanup cannot run during task use.
func stegoRunTasks(parent context.Context, tasks []stegoTask) error {
 ctx, cancel := context.WithCancel(parent)
 defer cancel()
 if ctx.Err() != nil { return nil }
 results := make(chan error, len(tasks))
 for _, task := range tasks {
  go func(task stegoTask) {
   returned := false
   var err error
   defer func() {
    // An explicit return flag also covers panic(nil) in legacy mode and Goexit.
    // Never retain, format, or unwrap the panic value.
    aborted := !returned
    if aborted { recover(); err = errors.New("task aborted before return") }
    if err == nil && ctx.Err() == nil {
     err = errors.New("task stopped before cancellation")
    }
    if err == context.Canceled && ctx.Err() != nil { err = nil }
    if err != nil { err = &stegoTaskFailure{name: task.name, cause: err, aborted: aborted} }
    results <- err
   }()
   err = task.run(ctx)
   returned = true
  }(task)
 }
 var failures []error
 for range tasks {
  if err := <-results; err != nil {
   cancel()
   failures = append(failures, err)
  }
 }
 return errors.Join(failures...)
}
`

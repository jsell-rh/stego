package controller

import (
	_ "embed"
	"fmt"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed testdata/runtime_test.go
var runtimeTests []byte

//go:embed testdata/pending_test.go
var pendingTests []byte

//go:embed testdata/keyed_test.go
var keyedTests []byte

//go:embed testdata/admission_test.go
var admissionTests []byte

//go:embed testdata/watch_keyed_test.go
var watchKeyedTests []byte

//go:embed testdata/sweep_test.go
var sweepTests []byte

//go:embed testdata/scan_test.go
var scanTests []byte

//go:embed testdata/sequence_test.go
var sequenceTests []byte

//go:embed testdata/stream_test.go
var streamTests []byte

//go:embed testdata/observation_test.go
var observationTests []byte

//go:embed testdata/metrics_test.go
var metricsTests []byte

//go:embed testdata/state_journal_test.go
var stateJournalTests []byte

//go:embed testdata/state_protection_test.go
var stateProtectionTests []byte

//go:embed testdata/checkpoint_test.go
var checkpointTests []byte

//go:embed testdata/cycle_test.go
var cycleTests []byte

//go:embed testdata/cycle_budget_test.go
var cycleBudgetTests []byte

//go:embed testdata/cycle_parallel_test.go
var cycleParallelTests []byte

//go:embed testdata/cycle_retry_test.go
var cycleRetryTests []byte

//go:embed testdata/telemetry_test.go
var telemetryTests []byte

//go:embed testdata/process_test.go
var processTests []byte

func TestGeneratedCycleParallel(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) { testGeneratedController(t, telemetry, "^TestCycleParallel") })
	}
}

func TestGeneratedCycleRetry(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) { testGeneratedController(t, telemetry, "^TestCycleRetry") })
	}
}

func TestGeneratedSweep(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) { testGeneratedController(t, telemetry, "^TestSweep") })
	}
}

func TestGeneratedCycleActionBudget(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) { testGeneratedController(t, telemetry, "^TestCycleActionBudget") })
	}
}

func TestGeneratedCycleWindow(t *testing.T) {
	testGeneratedController(t, false, "^TestCycleWindow")
}

func TestGeneratedControllerTraceBoundaries(t *testing.T) {
	testGeneratedController(t, true, "^TestControllerCleanupWorkTelemetry$")
}

func TestGeneratedPendingResult(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) {
			testGeneratedController(t, telemetry, "^(TestPendingResult|TestKey|TestMultipleKeyed|TestInvalidKeyed)")
		})
	}
}

func TestGeneratedController(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) { testGeneratedController(t, telemetry) })
	}
}
func TestGeneratedSequenceScan(t *testing.T) { testGeneratedController(t, false, "^TestSequenceScan") }

func TestGeneratedScanCheckpoint(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) {
			testGeneratedController(t, telemetry, "^(TestCheckpointedScan|TestScanRejectsUnstorable|TestScanCycle)")
		})
	}
}

func TestGeneratedStateJournal(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) { testGeneratedController(t, telemetry, "^TestStateJournal") })
	}
}
func TestGeneratedStateProtection(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) { testGeneratedController(t, telemetry, "^TestStateProtection") })
	}
}
func TestGeneratedControllerProcessTelemetry(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(fmt.Sprint(telemetry), func(t *testing.T) {
			testGeneratedController(t, telemetry, "^(TestMonitorOwnsSetupAndCleanupTelemetry|TestProcessSignalAndErrorPrivacy|TestProcessProbesDoNotStartApplication)$")
		})
	}
}

func testGeneratedController(t *testing.T, telemetry bool, patterns ...string) {
	ctx := gen.Context{OutputNamespace: "controller", ModuleName: "example.com/records", ServiceName: "records"}
	if telemetry {
		ctx.PeerNamespaces = map[string]string{"otel-tracing": "telemetry"}
	}
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, gen.File{Path: "controller/runtime_test.go", Content: runtimeTests}, gen.File{Path: "controller/keyed_test.go", Content: keyedTests}, gen.File{Path: "controller/sweep_test.go", Content: sweepTests})
	files = append(files, gen.File{Path: "controller/admission_test.go", Content: admissionTests}, gen.File{Path: "controller/pending_test.go", Content: pendingTests})
	files = append(files, gen.File{Path: "controller/stream_test.go", Content: streamTests}, gen.File{Path: "controller/observation_test.go", Content: observationTests})
	files = append(files, gen.File{Path: "controller/scan_test.go", Content: scanTests}, gen.File{Path: "controller/sequence_test.go", Content: sequenceTests}, gen.File{Path: "controller/watch_keyed_test.go", Content: watchKeyedTests})
	files = append(files, gen.File{Path: "controller/metrics_test.go", Content: metricsTests}, gen.File{Path: "controller/checkpoint_test.go", Content: checkpointTests})
	files = append(files, gen.File{Path: "controller/state_protection_test.go", Content: stateProtectionTests}, gen.File{Path: "controller/state_journal_test.go", Content: stateJournalTests})
	files = append(files, gen.File{Path: "controller/cycle_budget_test.go", Content: cycleBudgetTests})
	files = append(files, gen.File{Path: "controller/cycle_parallel_test.go", Content: cycleParallelTests})
	files = append(files, gen.File{Path: "controller/cycle_retry_test.go", Content: cycleRetryTests})
	files = append(files, gen.File{Path: "controller/cycle_test.go", Content: cycleTests}, gen.File{Path: "controller/process_test.go", Content: processTests})
	var module strings.Builder
	module.WriteString("module example.com/records\n")
	if telemetry {
		module.WriteString("go 1.26.0\n")
	} else {
		module.WriteString("go 1.25.0\n")
	}
	if telemetry {
		ctx.OutputNamespace = "telemetry"
		generated, wiring, err := new(oteltracing.Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, generated...)
		files = append(files, gen.File{Path: "controller/telemetry_test.go", Content: telemetryTests})
		var names []string
		for name := range wiring.GoModRequires {
			names = append(names, name)
		}
		sort.Strings(names)
		module.WriteString("require (\n")
		for _, name := range names {
			fmt.Fprintf(&module, "%s %s\n", name, wiring.GoModRequires[name])
		}
		module.WriteString(")\n")
	}
	project := t.TempDir()
	for _, file := range files {
		name := filepath.Join(project, file.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(module.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if telemetry {
		cmd := exec.Command("go", "mod", "tidy")
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("resolve telemetry modules: %v %s", err, output)
		}
	}
	cmd := exec.Command("go", "test", "-race", "-count=1", "-timeout=45s", "./...")
	if len(patterns) != 0 {
		cmd.Args = append(cmd.Args, "-run="+strings.Join(patterns, "|"))
	}
	retryCheck := len(patterns) == 1 && patterns[0] == "^TestCycleRetry"
	traceCheck := len(patterns) == 1 && patterns[0] == "^TestControllerCleanupWorkTelemetry$"
	if retryCheck || traceCheck {
		cmd.Args = append(cmd.Args, "-v")
	}
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated controller: %v\n%s", err, output)
	} else if traceCheck {
		for _, name := range []string{"TestControllerCleanupWorkTelemetry", "TestControllerCleanupWorkTelemetry/false", "TestControllerCleanupWorkTelemetry/true"} {
			if !strings.Contains(string(output), "--- PASS: "+name+" ") {
				t.Fatalf("required cleanup result is absent: %s\n%s", name, output)
			}
		}
		if strings.Contains(string(output), "--- SKIP:") {
			t.Fatalf("cleanup check skipped a case: %s", output)
		}
		t.Logf("generated cleanup trace results:\n%s", output)
	} else if retryCheck {
		for _, name := range []string{"ResolvesCurrentFailureBeforeCursorAdvance", "LimitsAndErrorSelection", "DeadlineRequiresFullReserve", "CancellationStopsDelayAndSave", "DoesNotRetryCheckpointConflict", "RejectsInvalidPolicyBeforeStorage", "RetainsKeyOrderWithIndependentWork", "PeerFailureCancelsWaitingRetry"} {
			if !strings.Contains(string(output), "--- PASS: TestCycleRetry"+name+" ") {
				t.Fatalf("required generated retry result is absent: %s\n%s", name, output)
			}
		}
		if strings.Contains(string(output), "--- SKIP:") {
			t.Fatalf("generated retry check skipped a case:\n%s", output)
		}
		t.Logf("generated retry results:\n%s", output)
	}
	if os.Getenv("STEGO_STRESS_CONTROLLER") == "1" {
		for _, procs := range []string{"1", "4"} {
			stress := exec.Command("go", "test", "-race", "-count=100", "-timeout=30s", "-run=^TestKeyedWatchReconnectJoinsActionsAndRepeatsDiscovery$", "./controller")
			stress.Dir = project
			stress.Env = append(os.Environ(), "GOWORK=off", "GOMAXPROCS="+procs)
			if output, err := stress.CombinedOutput(); err != nil {
				t.Fatalf("reconnect stress with GOMAXPROCS=%s: %v\n%s", procs, err, output)
			}
			t.Logf("reconnect stress passed: GOMAXPROCS=%s, 100 runs", procs)
		}
	}
	if os.Getenv("STEGO_BENCH_CONTROLLER") == "1" {
		bench := exec.Command("go", "test", "-run=^$", "-bench=^Benchmark(KeyQueueWorkers|KeyAdmission|StreamScan|ControllerMetrics|KeyReconnect)$", "-benchtime=200ms", "-count=3", "./...")
		bench.Dir = project
		bench.Env = append(os.Environ(), "GOWORK=off")
		output, err := bench.CombinedOutput()
		if err != nil {
			t.Fatalf("generated controller benchmark: %v\n%s", err, output)
		}
		t.Logf("generated controller benchmark:\n%s", output)
	}
	if os.Getenv("STEGO_BENCH_OBSERVATION") == "1" {
		bench := exec.Command("go", "test", "-run=^$", "-bench=^BenchmarkObservation$", "-benchtime=200ms", "-count=3", "./...")
		bench.Dir = project
		bench.Env = append(os.Environ(), "GOWORK=off")
		output, err := bench.CombinedOutput()
		if err != nil {
			t.Fatalf("generated observation benchmark: %v\n%s", err, output)
		}
		t.Logf("generated observation benchmark:\n%s", output)
	}
}
func TestRejectInvalidGeneration(t *testing.T) {
	for _, ctx := range []gen.Context{{}, {OutputNamespace: "../escape"}, {OutputNamespace: "controller", ComponentConfig: map[string]any{"workers": 0}}, {OutputNamespace: "controller", PeerNamespaces: map[string]string{"otel-tracing": "../outside"}}, {OutputNamespace: "controller", PeerNamespaces: map[string]string{"otel-tracing": "tracing"}}} {
		if _, _, err := new(Generator).Generate(ctx); err == nil {
			t.Fatal("invalid generation accepted")
		}
	}
}

func TestNestedControllerLibraryCanBeImported(t *testing.T) {
	for _, namespace := range []string{"go-services/worker", "libs/init", "libs/string"} {
		t.Run(namespace, func(t *testing.T) {
			files, _, err := new(Generator).Generate(gen.Context{OutputNamespace: namespace})
			if err != nil {
				t.Fatal(err)
			}
			project := t.TempDir()
			files = append(files, gen.File{Path: "go.mod", Content: []byte("module example.com/controllernamespace\ngo 1.26.8\n")}, gen.File{Path: "main.go", Content: []byte("package main\nimport runtime \"example.com/controllernamespace/" + namespace + "\"\nfunc main(){_=runtime.ErrScanContract}\n")})
			for _, file := range files {
				name := filepath.Join(project, file.Path)
				if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, file.Content, 0644); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command("go", "build", "-mod=readonly", "./...")
			command.Dir = project
			command.Env = append(os.Environ(), "GOWORK=off")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("nested library import failed: %v\n%s", err, output)
			}
		})
	}
}

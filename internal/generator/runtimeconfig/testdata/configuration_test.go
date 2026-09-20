package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfigurationDefaultsAndSingleRead(t *testing.T) {
	seen := map[string]int{}
	result, err := ReadWorker(func(name string) (string, bool) {
		seen[name]++
		return "server.example:443", name == "WIDGET_ADDRESS"
	})
	if err != nil || result.Address != "server.example:443" || result.WatchLimit != 0 || result.Resync != 30*time.Second || result.Enabled || result.Optional != "" {
		t.Fatal("incorrect default settings", err)
	}
	if len(seen) != 5 {
		t.Fatal("unexpected environment lookup")
	}
	for _, count := range seen {
		if count != 1 {
			t.Fatal("repeated lookup")
		}
	}
	storage, err := ReadStorage(func(name string) (string, bool) {
		if name != "WIDGET_ADDRESS" {
			t.Fatal("cross-group read")
		}
		return "database", true
	})
	if err != nil || storage.Address != "database" {
		t.Fatal("separate group failed")
	}
}

func TestConfigurationRejectsValuesWithoutPartialResults(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"missing":                 {},
		"empty required":          {"WIDGET_ADDRESS": ""},
		"control":                 {"WIDGET_ADDRESS": "private-sentinel\n"},
		"invalid unicode":         {"WIDGET_ADDRESS": string([]byte{0xff})},
		"too long":                {"WIDGET_ADDRESS": strings.Repeat("x", 257)},
		"private integer":         {"WIDGET_ADDRESS": "private-sentinel", "WIDGET_WATCH_LIMIT": "private-sentinel"},
		"integer overflow":        {"WIDGET_ADDRESS": "private-sentinel", "WIDGET_WATCH_LIMIT": "9223372036854775808"},
		"integer bounds":          {"WIDGET_ADDRESS": "private-sentinel", "WIDGET_WATCH_LIMIT": "1001"},
		"integer negative":        {"WIDGET_ADDRESS": "private-sentinel", "WIDGET_WATCH_LIMIT": "-1"},
		"integer spelling":        {"WIDGET_ADDRESS": "private-sentinel", "WIDGET_WATCH_LIMIT": "01"},
		"integer whitespace":      {"WIDGET_ADDRESS": "private-sentinel", "WIDGET_WATCH_LIMIT": " 1"},
		"empty overrides default": {"WIDGET_ADDRESS": "private-sentinel", "WIDGET_RESYNC": ""},
		"duration overflow":       {"WIDGET_ADDRESS": "private-sentinel", "WIDGET_RESYNC": "999999999999h"},
		"duration small":          {"WIDGET_ADDRESS": "private-sentinel", "WIDGET_RESYNC": "1ns"},
		"duration large":          {"WIDGET_ADDRESS": "private-sentinel", "WIDGET_RESYNC": "2h"},
		"duration long":           {"WIDGET_ADDRESS": "private-sentinel", "WIDGET_RESYNC": strings.Repeat("0", 65) + "s"},
		"boolean spelling":        {"WIDGET_ADDRESS": "private-sentinel", "WIDGET_ENABLED": "TRUE"},
		"optional control":        {"WIDGET_ADDRESS": "private-sentinel", "WIDGET_OPTIONAL": "\x00"},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := ReadWorker(func(name string) (string, bool) { v, ok := values[name]; return v, ok })
			var detail *Error
			if err == nil || !errors.Is(err, ErrConfiguration) || !errors.As(err, &detail) || result != (Worker{}) {
				t.Fatal("invalid settings were returned")
			}
			if detail.Group() != "Worker" || detail.Field() == "" || (detail.Reason() != "invalid" && detail.Reason() != "missing") {
				t.Fatal("invalid error classification")
			}
			for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
				if strings.Contains(fmt.Sprintf(format, err), "private-sentinel") {
					t.Fatal("error exposed input")
				}
			}
		})
	}
	result, err := ReadWorker(nil)
	if !errors.Is(err, ErrConfiguration) || result != (Worker{}) {
		t.Fatal("nil lookup accepted")
	}
}

func TestConfigurationBoundsAndExplicitEmpty(t *testing.T) {
	for _, values := range []map[string]string{
		{"WIDGET_ADDRESS": "a", "WIDGET_WATCH_LIMIT": "0", "WIDGET_RESYNC": "1ms", "WIDGET_ENABLED": "false", "WIDGET_OPTIONAL": ""},
		{"WIDGET_ADDRESS": strings.Repeat("a", 256), "WIDGET_WATCH_LIMIT": "1000", "WIDGET_RESYNC": "1h", "WIDGET_ENABLED": "true", "WIDGET_OPTIONAL": strings.Repeat("é", 16)},
	} {
		result, err := ReadWorker(func(name string) (string, bool) { v, ok := values[name]; return v, ok })
		if err != nil || result.Address != values["WIDGET_ADDRESS"] || result.Optional != values["WIDGET_OPTIONAL"] {
			t.Fatal("valid boundary rejected", err)
		}
	}
	for _, value := range []string{"-9223372036854775808", "9223372036854775807"} {
		if _, ok := readInteger(value, -9223372036854775808, 9223372036854775807); !ok {
			t.Fatal("int64 boundary rejected")
		}
	}
}

func TestConfigurationFormattingIsPrivate(t *testing.T) {
	result, err := ReadWorker(func(name string) (string, bool) { return "private-sentinel", name == "WIDGET_ADDRESS" })
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{result, &result, []Worker{result}, map[string]Worker{"worker": result}} {
		for _, format := range []string{"%s", "%v", "%+v", "%#v", "%q"} {
			if strings.Contains(fmt.Sprintf(format, v), "private-sentinel") {
				t.Fatal("formatting exposed settings")
			}
		}
		if raw, err := json.Marshal(v); err == nil || strings.Contains(string(raw), "private-sentinel") {
			t.Fatal("JSON export accepted")
		}
	}
}

func TestConfigurationReadsHaveNoSharedState(t *testing.T) {
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := 0; j < 10; j++ {
				result, err := ReadWorker(func(name string) (string, bool) { return "worker", name == "WIDGET_ADDRESS" })
				if err != nil || result.Address != "worker" {
					t.Error("concurrent read failed")
				}
			}
		}()
	}
	wait.Wait()
}

func TestConfigurationEnvironmentLoader(t *testing.T) {
	t.Setenv("WIDGET_ADDRESS", "worker")
	t.Setenv("WIDGET_WATCH_LIMIT", "2")
	t.Setenv("WIDGET_RESYNC", "1.5s")
	t.Setenv("WIDGET_ENABLED", "true")
	t.Setenv("WIDGET_OPTIONAL", "")
	got, err := LoadWorker()
	want := Worker{Address: "worker", WatchLimit: 2, Resync: 1500 * time.Millisecond, Enabled: true}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("environment load failed", err)
	}
}

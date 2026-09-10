package controller

import (
	"context"
	"testing"

	tracing "example.com/records/telemetry"
)

func TestOverlappingControllersShareProviderOwnership(t *testing.T) {
	first, closeFirst, err := startControllerTelemetry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeFirst()
	second, closeSecond, err := startControllerTelemetry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeSecond()
	if first.Value(controllerTelemetryKey{}) != second.Value(controllerTelemetryKey{}) {
		t.Fatal("overlapping controllers created separate metric streams")
	}
	provider := controllerTelemetryOwner.runtime
	detachFirst, err := attachControllerQueue(first, func() QueueMetrics { return QueueMetrics{Capacity: 2} })
	if err != nil {
		t.Fatal(err)
	}
	detachSecond, err := attachControllerQueue(second, func() QueueMetrics { return QueueMetrics{Capacity: 3} })
	if err != nil {
		t.Fatal(err)
	}
	detachFirst()
	closeFirst()
	closeFirst()
	if !provider.LogServiceEvent(context.Background(), tracing.ServiceReady) {
		t.Fatal("first controller closed a shared provider")
	}
	detachSecond()
	closeSecond()
	if provider.LogServiceEvent(context.Background(), tracing.ServiceReady) {
		t.Fatal("last controller did not close its provider")
	}
	if controllerTelemetryOwner.runtime != nil || controllerTelemetryOwner.users != 0 {
		t.Fatal("controller provider ownership was retained")
	}
}

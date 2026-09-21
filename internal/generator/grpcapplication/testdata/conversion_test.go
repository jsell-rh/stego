package transport_test

import (
	"errors"
	"testing"
	"time"

	transport "example.com/timestamp-test/out/CONVERSION_NAMESPACE/transport"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTimestampSupportedInstants(t *testing.T) {
	plusHour := time.FixedZone("source-zone", 3600)
	for _, tc := range []struct {
		name    string
		value   time.Time
		seconds int64
		nanos   int32
	}{
		{"zero-is-present", time.Time{}, -62135596800, 0},
		{"first", time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), -62135596800, 0},
		{"last", time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), 253402300799, 999999999},
		{"epoch", time.Unix(0, 0), 0, 0},
		{"before-epoch", time.Unix(-1, 999999999), -1, 999999999},
		{"first-local-zone", time.Date(1, 1, 1, 1, 0, 0, 0, plusHour), -62135596800, 0},
		{"last-local-zone", time.Date(10000, 1, 1, 0, 59, 59, 999999999, plusHour), 253402300799, 999999999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := transport.Timestamp(tc.value)
			if err != nil || got == nil || got.Seconds != tc.seconds || got.Nanos != tc.nanos {
				t.Fatalf("supported instant was not preserved: %v", err)
			}
			// Check the actual protobuf wire and JSON codecs used by consumers.
			wire, err := proto.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var decoded timestamppb.Timestamp
			if err := proto.Unmarshal(wire, &decoded); err != nil || !decoded.AsTime().Equal(tc.value) {
				t.Fatal("protobuf round trip changed the instant")
			}
			encoded, err := protojson.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if err := protojson.Unmarshal(encoded, &decoded); err != nil || !decoded.AsTime().Equal(tc.value) {
				t.Fatal("JSON round trip changed the instant")
			}
		})
	}
}

func TestTimestampRejectsInvalidInstantsWithoutOutput(t *testing.T) {
	first := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	for _, tc := range []struct {
		name  string
		value time.Time
	}{
		{"before-first", first.Add(-time.Nanosecond)},
		{"after-last", last.Add(time.Nanosecond)},
		{"negative-year", time.Date(-10000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"large-year", time.Date(100000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"local-first-outside-UTC", time.Date(1, 1, 1, 0, 0, 0, 0, time.FixedZone("private-zone", 3600))},
		{"local-last-outside-UTC", time.Date(9999, 12, 31, 23, 59, 59, 0, time.FixedZone("private-zone", -3600))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, convert := range []func() (*timestamppb.Timestamp, error){
				func() (*timestamppb.Timestamp, error) { return transport.Timestamp(tc.value) },
				func() (*timestamppb.Timestamp, error) { return transport.OptionalTimestamp(&tc.value) },
			} {
				got, err := convert()
				if got != nil || !errors.Is(err, transport.ErrTimestamp) || err.Error() != "invalid protobuf timestamp" {
					t.Fatal("invalid instant returned data or an error with supplied values")
				}
			}
		})
	}
}

func TestOptionalTimestampPreservesPresenceAndOwnsOutput(t *testing.T) {
	missing, err := transport.OptionalTimestamp(nil)
	if missing != nil || err != nil {
		t.Fatal("absent instant became present")
	}
	value := time.Time{}
	first, err := transport.OptionalTimestamp(&value)
	if first == nil || err != nil {
		t.Fatal("present zero instant was lost")
	}
	second, err := transport.OptionalTimestamp(&value)
	if second == nil || err != nil || first == second {
		t.Fatal("results share mutable protobuf data")
	}
	first.Seconds = 0
	value = time.Unix(10, 20)
	if second.Seconds != -62135596800 || second.Nanos != 0 {
		t.Fatal("result changed after input or sibling mutation")
	}
}

func TestTimestampDiscardsMonotonicState(t *testing.T) {
	value := time.Now()
	got, err := transport.Timestamp(value)
	if err != nil || got == nil || !got.AsTime().Equal(value) {
		t.Fatal("wall-clock instant changed")
	}
	if got.AsTime().Location() != time.UTC {
		t.Fatal("timestamp retained a local zone")
	}
}

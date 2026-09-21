package mapping_test

import (
	"bytes"
	"errors"
	"math"
	"testing"
	"time"

	"example.com/mapping-test/out/MAPPING_NAMESPACE/mapping"
	"example.com/mapping-test/out/store"
	"google.golang.org/protobuf/proto"
)

func sample() store.Shipment {
	name := ""
	recorded := time.Date(2026, 9, 21, 1, 2, 3, 456, time.FixedZone("offset", 3600))
	limit := int64(42)
	return store.Shipment{Meta: store.Meta{ID: "parcel-1", CreatedTime: recorded, UpdatedTime: recorded}, Name: &name, Count: 42, Enabled: true, Content: []byte("cargo"), RecordedAt: &recorded, Score: 1.25, Total: 123.5, Limit: &limit}
}

func TestMappingPreservesContract(t *testing.T) {
	input := sample()
	result, err := mapping.Shipment(input)
	if err != nil || result == nil {
		t.Fatal("valid input rejected", err)
	}
	if result.Metadata.Id != "parcel-1" || result.Metadata.Kind != "Shipment" || result.Metadata.Href != "/shipments/parcel-1" {
		t.Fatal("metadata differs", result.Metadata)
	}
	if result.Metadata.CreatedAt.CheckValid() != nil || !result.Metadata.CreatedAt.AsTime().Equal(input.CreatedTime) || !result.Metadata.UpdatedAt.AsTime().Equal(input.UpdatedTime) || !result.RecordedAt.AsTime().Equal(*input.RecordedAt) {
		t.Fatal("timestamp differs")
	}
	if result.Name == nil || *result.Name != "" || !result.ProtoReflect().Has(result.ProtoReflect().Descriptor().Fields().ByName("reset")) || result.ProtoReflect().Get(result.ProtoReflect().Descriptor().Fields().ByName("reset")).String() != "" {
		t.Fatal("empty present value became absent")
	}
	if result.Count != 42 || !result.Enabled || !bytes.Equal(result.Content, input.Content) || result.Score != 1.25 || result.Total != 123.5 || result.Limit == nil || *result.Limit != 42 || result.InternalTags != nil {
		t.Fatal("scalar or omitted field differs")
	}
	encoded, err := proto.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	decoded := result.ProtoReflect().New().Interface()
	if err := proto.Unmarshal(encoded, decoded); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(result, decoded) {
		t.Fatal("protobuf round trip differs")
	}
}

func TestMappingOwnsOutputAndPreservesAbsence(t *testing.T) {
	input := sample()
	result, err := mapping.Shipment(input)
	if err != nil {
		t.Fatal(err)
	}
	*result.Name = "changed"
	result.Content[0] = 'X'
	result.RecordedAt.Seconds++
	*result.Limit = 1
	if *input.Name != "" || string(input.Content) != "cargo" || input.RecordedAt.Second() != 3 || *input.Limit != 42 {
		t.Fatal("output retains input memory")
	}
	input.Name = nil
	input.RecordedAt = nil
	input.Content = nil
	input.Limit = nil
	result, err = mapping.Shipment(input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != nil || result.RecordedAt != nil || result.Content != nil || result.Limit != nil {
		t.Fatal("absent value became present")
	}
	input.Content = []byte{}
	result, err = mapping.Shipment(input)
	if err != nil || result.Content == nil {
		t.Fatal("present empty bytes became nil", err)
	}
}

func TestMappingRejectsInvalidValuesWithoutPartialOutput(t *testing.T) {
	for name, change := range map[string]func(*store.Shipment){
		"invalid text":          func(s *store.Shipment) { s.Reset = string([]byte{0xff}) },
		"invalid optional text": func(s *store.Shipment) { v := string([]byte{0xff}); s.Name = &v },
		"created":               func(s *store.Shipment) { s.CreatedTime = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC) },
		"updated":               func(s *store.Shipment) { s.UpdatedTime = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
		"optional time":         func(s *store.Shipment) { v := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC); s.RecordedAt = &v },
		"count high":            func(s *store.Shipment) { s.Count = math.MaxInt32 + 1 },
		"count low":             func(s *store.Shipment) { s.Count = math.MinInt32 - 1 },
		"optional count":        func(s *store.Shipment) { v := int64(math.MaxInt32) + 1; s.Limit = &v },
	} {
		t.Run(name, func(t *testing.T) {
			input := sample()
			change(&input)
			result, err := mapping.Shipment(input)
			if result != nil || !errors.Is(err, mapping.ErrConversion) || err.Error() != "response conversion failed" {
				t.Fatal("invalid input returned output or a private value", result, err)
			}
		})
	}
}

func TestMappingAcceptsNumericAndTimeBounds(t *testing.T) {
	for _, count := range []int64{math.MinInt32, math.MaxInt32, 0} {
		input := sample()
		input.Count = count
		input.Limit = &count
		input.CreatedTime = time.Time{}
		input.UpdatedTime = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
		result, err := mapping.Shipment(input)
		if err != nil || int64(result.Count) != count || int64(*result.Limit) != count || !result.Metadata.CreatedAt.AsTime().IsZero() || !result.Metadata.UpdatedAt.AsTime().Equal(input.UpdatedTime) {
			t.Fatal("valid bound rejected", err)
		}
	}
}

package mapping

import (
	model "example.com/mapping-test/out/store"
	"google.golang.org/protobuf/proto"
	"math"
	"reflect"
	"testing"
	"time"
)

func preparedValue() ShipmentInput {
	display := "resolved carrier"
	limit := int64(7)
	recorded := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	return ShipmentInput{Reference: "resolved-reference", Created: recorded, Display: &display, Count: 17, Enabled: true, Recorded: &recorded, Reset: "", Score: 1.25, Total: 2.5, Limit: &limit, Tags: []byte(`["second","first","second"]`), PreparedCount: 23}
}
func TestPreparedMappingUsesApplicationValues(t *testing.T) {
	row := model.Shipment{}
	row.ID = "stored-id"
	row.CreatedTime = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	input := preparedValue()
	got, err := Shipment(row, input)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata.Id != "stored-id" || got.Metadata.Href != "/shipments/resolved-reference" || got.Metadata.Kind != "Shipment" || !got.Metadata.CreatedAt.AsTime().Equal(input.Created) || got.GetName() != *input.Display || got.Count != 17 || !got.Enabled || got.RecordedAt == nil || !got.RecordedAt.AsTime().Equal(*input.Recorded) || got.Score != 1.25 || got.Total != 2.5 || got.GetLimit() != 7 || got.PreparedCount != 23 || !reflect.DeepEqual(got.Tags, []string{"second", "first", "second"}) {
		t.Fatal("prepared response fields differ", got)
	}
	reset := got.ProtoReflect().Descriptor().Fields().ByName("reset")
	if !got.ProtoReflect().Has(reset) || got.ProtoReflect().Get(reset).String() != "" {
		t.Fatal("prepared empty value lost field presence")
	}
	if _, err = proto.Marshal(got); err != nil {
		t.Fatal(err)
	}
	input.Display = nil
	input.Recorded = nil
	input.Limit = nil
	input.Tags = nil
	got, err = Shipment(row, input)
	if err != nil || got.Name != nil || got.RecordedAt != nil || got.Limit != nil || got.Tags != nil {
		t.Fatal("optional input presence changed", err)
	}
}
func TestPreparedMappingOwnsResults(t *testing.T) {
	input := preparedValue()
	got, err := Shipment(model.Shipment{}, input)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name == input.Display {
		t.Fatal("response shares an input pointer")
	}
	*got.Name = "changed"
	if *input.Display != "resolved carrier" {
		t.Fatal("response changed input")
	}
	*input.Limit = 99
	input.Tags[2] = 'X'
	*input.Recorded = time.Time{}
	if got.GetLimit() != 7 || got.Tags[0] != "second" || got.RecordedAt.AsTime().IsZero() {
		t.Fatal("input changed response")
	}
}
func TestPreparedMappingRejectsInvalidValues(t *testing.T) {
	for name, edit := range map[string]func(*ShipmentInput){
		"utf8":             func(v *ShipmentInput) { v.Reference = string([]byte{255}) },
		"optional utf8":    func(v *ShipmentInput) { s := string([]byte{255}); v.Display = &s },
		"created time":     func(v *ShipmentInput) { v.Created = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
		"optional time":    func(v *ShipmentInput) { x := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC); v.Recorded = &x },
		"integer":          func(v *ShipmentInput) { v.Count = math.MaxInt32 + 1 },
		"optional integer": func(v *ShipmentInput) { x := int64(math.MinInt32) - 1; v.Limit = &x },
		"list":             func(v *ShipmentInput) { v.Tags = []byte(`["first",null]`) },
		"list limit":       func(v *ShipmentInput) { v.Tags = []byte(`["1","2","3","4"]`) },
	} {
		t.Run(name, func(t *testing.T) {
			input := preparedValue()
			edit(&input)
			got, err := Shipment(model.Shipment{}, input)
			if got != nil || err != ErrConversion || err.Error() != "response conversion failed" {
				t.Fatal("invalid prepared input returned output or another error")
			}
		})
	}
}

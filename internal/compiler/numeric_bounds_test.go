package compiler

import (
	"github.com/jsell-rh/stego/internal/types"
	"math"
	"strings"
	"testing"
)

func TestNumericBoundsMustBeFinite(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		for _, minimum := range []bool{true, false} {
			field := types.Field{Name: "score", Type: types.FieldTypeDouble}
			if minimum {
				field.Min = &value
			} else {
				field.Max = &value
			}
			found := false
			for _, err := range validateFieldTypes([]types.Entity{{Name: "Record", Fields: []types.Field{field}}}) {
				if strings.Contains(err.Message, "numeric bounds must be finite") {
					found = true
				}
			}
			if !found {
				t.Fatal("non-finite numeric bound was accepted", value, minimum)
			}
		}
	}
}

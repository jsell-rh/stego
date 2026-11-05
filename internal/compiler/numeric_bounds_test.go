package compiler

import (
	"math"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/types"
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
			for _, err := range declaration.ValidateFieldTypes([]types.Entity{{Name: "Record", Fields: []types.Field{field}}}) {
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

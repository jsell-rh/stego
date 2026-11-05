package declaration

import (
	"math"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/types"
)

func TestComponentConfigValues(t *testing.T) {
	t.Run("ST-DECL-INV36-T A setting value must suit its schema field", func(t *testing.T) {
		stringList := types.ConfigField{Type: "list", Items: &types.ConfigFieldItems{Inline: &types.ConfigField{Type: "string"}}}
		objectList := types.ConfigField{Type: "list", Items: &types.ConfigFieldItems{Fields: map[string]types.ConfigField{
			"name": {Type: "string"}, "enabled": {Type: "bool", Optional: true},
		}}}
		tests := []struct {
			name  string
			field types.ConfigField
			value any
			want  string
		}{
			{"string", types.ConfigField{Type: "string"}, "token", ""},
			{"wrong string", types.ConfigField{Type: "string"}, 1, "type string"},
			{"wrong bool", types.ConfigField{Type: "bool"}, "false", "type bool"},
			{"integer", types.ConfigField{Type: "int"}, 42, ""},
			{"fraction", types.ConfigField{Type: "int"}, 0.5, "type int"},
			{"not finite", types.ConfigField{Type: "number"}, math.Inf(1), "type number"},
			{"enum", types.ConfigField{Enum: []string{"a", "b"}}, "a", ""},
			{"unknown enum", types.ConfigField{Enum: []string{"a", "b"}}, "c", "one of"},
			{"null", types.ConfigField{Type: "string", Optional: true}, nil, "must not be null"},
			{"list", stringList, []any{"a", "b"}, ""},
			{"wrong item", stringList, []any{"a", 2}, "config[1]"},
			{"wrong list", stringList, "a,b", "type list"},
			{"object", objectList, []any{map[string]any{"name": "test", "enabled": false}}, ""},
			{"unknown nested key", objectList, []any{map[string]any{"name": "test", "enbaled": false}}, "config[0].enbaled"},
			{"missing nested key", objectList, []any{map[string]any{"enabled": false}}, "config[0].name"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				errors := ValidateSettingValue("config", tt.value, tt.field)
				if tt.want == "" {
					if len(errors) != 0 {
						t.Fatalf("unexpected errors: %+v", errors)
					}
				} else if len(errors) == 0 || !strings.Contains(errors[0].Message, tt.want) {
					t.Fatalf("got %+v, want %q", errors, tt.want)
				}
			})
		}
	})
}

package spicedbauthz

import "testing"

func TestSchemaPathForm(t *testing.T) {
	t.Run("ST-SUP-RC29-OC13-T03 A schema path with a control character is refused before any file is written", func(t *testing.T) {
		for _, name := range []string{"authz/schema.zed\nvar _ = 1", "authz/sch\rema.zed", "authz/schema\x00.zed"} {
			ctx := documentContext(t)
			ctx.Authorization.SchemaPath = name
			ctx.Inputs = map[string][]byte{name: []byte(documentSchema)}
			refused(t, ctx, "the schema")
		}
	})
}

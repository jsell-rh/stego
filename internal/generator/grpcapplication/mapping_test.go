package grpcapplication_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/types"
)

const shipmentProto = `syntax="proto3"; package shipping.v1;
import "google/protobuf/timestamp.proto";
message Metadata {string id=1; string kind=2; string href=3; google.protobuf.Timestamp created_at=4; google.protobuf.Timestamp updated_at=5;}
message Shipment {
 Metadata metadata=1; optional string name=2; int32 count=3; bool enabled=4;
 bytes content=5; google.protobuf.Timestamp recorded_at=6; optional string reset=7;
 repeated string internal_tags=8; float score=9; double total=10; optional int32 limit=11; repeated string tags=12;
}
`

func shipmentContext(t *testing.T) gen.Context {
	t.Helper()
	ctx := gen.Context{ModuleName: "example.com/mapping-test", OutDirName: "out", OutputNamespace: "store",
		StorageContract: "example.com/mapping-test/out/contracts/storage", AuthPackage: "example.com/mapping-test/out/auth",
		PeerNamespaces: map[string]string{"jwt-auth": "auth"},
		Entities: []types.Entity{{Name: "Shipment", Fields: []types.Field{
			{Name: "name", Type: types.FieldTypeString, Optional: true}, {Name: "count", Type: types.FieldTypeInt64},
			{Name: "enabled", Type: types.FieldTypeBool}, {Name: "content", Type: types.FieldTypeBytes},
			{Name: "recorded_at", Type: types.FieldTypeTimestamp, Optional: true}, {Name: "reset", Type: types.FieldTypeString},
			{Name: "score", Type: types.FieldTypeFloat}, {Name: "total", Type: types.FieldTypeDouble},
			{Name: "limit", Type: types.FieldTypeInt64, Optional: true},
			{Name: "tags", Type: types.FieldTypeJsonb, Optional: true},
		}}}, Inputs: map[string][]byte{"api.proto": []byte(shipmentProto)}}
	models, err := new(postgresadapter.Generator).GoModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx.GoModelSources = map[string]gen.GoModelSource{"custom-store": {ImportPath: "example.com/mapping-test/out/store", Models: models}}
	ctx.OutputNamespace = "grpcapi"
	ctx.ComponentConfig = map[string]any{"factory_package": "application", "proto_files": []any{map[string]any{"path": "api.proto", "import_path": "shipping/v1/api.proto"}},
		"response_mappings": []any{map[string]any{"name": "Shipment", "provider": "custom-store", "model": "Shipment", "message": "shipping.v1.Shipment", "fields": []any{
			map[string]any{"target": "metadata.id", "source": "id"},
			map[string]any{"target": "metadata.kind", "constant": "Shipment"},
			map[string]any{"target": "metadata.href", "source": "id", "prefix": "/shipments/"},
			map[string]any{"target": "metadata.created_at", "source": "created_time"},
			map[string]any{"target": "metadata.updated_at", "source": "updated_time"},
			map[string]any{"target": "name", "source": "name"},
			map[string]any{"target": "count", "source": "count", "conversion": "int32"},
			map[string]any{"target": "enabled", "source": "enabled"},
			map[string]any{"target": "content", "source": "content"},
			map[string]any{"target": "recorded_at", "source": "recorded_at"},
			map[string]any{"target": "reset", "source": "reset"},
			map[string]any{"target": "internal_tags", "omit": true},
			map[string]any{"target": "score", "source": "score"},
			map[string]any{"target": "total", "source": "total"},
			map[string]any{"target": "limit", "source": "limit", "conversion": "int32"},
			map[string]any{"target": "tags", "source": "tags", "conversion": "json_strings", "max_bytes": 64, "max_items": 3, "max_item_bytes": 16},
		}}}}
	return ctx
}

func mappingDeclaration(ctx gen.Context) map[string]any {
	return ctx.ComponentConfig["response_mappings"].([]any)[0].(map[string]any)
}
func mappingRule(ctx gen.Context, target string) map[string]any {
	for _, item := range mappingDeclaration(ctx)["fields"].([]any) {
		rule := item.(map[string]any)
		if rule["target"] == target {
			return rule
		}
	}
	panic("missing fixture rule")
}

func TestResponseMappingRejectsInvalidContracts(t *testing.T) {
	for name, change := range map[string]func(gen.Context){
		"missing field": func(c gen.Context) { mappingRule(c, "name")["target"] = "absent" },
		"new public field": func(c gen.Context) {
			c.Inputs["api.proto"] = []byte(strings.Replace(shipmentProto, "int32 count=3;", "int32 count=3; string added=15;", 1))
		},
		"unknown source":   func(c gen.Context) { mappingRule(c, "name")["source"] = "secret" },
		"unknown provider": func(c gen.Context) { mappingDeclaration(c)["provider"] = "missing" },
		"unknown model":    func(c gen.Context) { mappingDeclaration(c)["model"] = "missing" },
		"unknown message":  func(c gen.Context) { mappingDeclaration(c)["message"] = "missing" },
		"unknown option":   func(c gen.Context) { mappingRule(c, "name")["typo"] = true },
		"duplicate target": func(c gen.Context) {
			d := mappingDeclaration(c)
			d["fields"] = append(d["fields"].([]any), mappingRule(c, "name"))
		},
		"duplicate function": func(c gen.Context) {
			c.ComponentConfig["response_mappings"] = append(c.ComponentConfig["response_mappings"].([]any), mappingDeclaration(c))
		},
		"reserved function":   func(c gen.Context) { mappingDeclaration(c)["name"] = "ErrConversion" },
		"code expression":     func(c gen.Context) { mappingDeclaration(c)["name"] = "Make()" },
		"private model":       func(c gen.Context) { c.GoModelSources["custom-store"].Models[0].GoType = "private" },
		"private selector":    func(c gen.Context) { c.GoModelSources["custom-store"].Models[0].Fields[0].Selection = "private" },
		"selector expression": func(c gen.Context) { c.GoModelSources["custom-store"].Models[0].Fields[0].Selection = "ID()" },
		"wrong type":          func(c gen.Context) { mappingRule(c, "name")["source"] = "enabled" },
		"presence loss": func(c gen.Context) {
			c.Inputs["api.proto"] = []byte(strings.Replace(shipmentProto, "optional string name", "string name", 1))
		},
		"implicit narrowing":   func(c gen.Context) { delete(mappingRule(c, "count"), "conversion") },
		"unknown conversion":   func(c gen.Context) { mappingRule(c, "count")["conversion"] = "cast" },
		"optional prefix":      func(c gen.Context) { mappingRule(c, "name")["prefix"] = "name:" },
		"constant plus source": func(c gen.Context) { mappingRule(c, "name")["constant"] = "secret" },
		"false omission":       func(c gen.Context) { mappingRule(c, "internal_tags")["omit"] = false },
		"omission plus source": func(c gen.Context) { mappingRule(c, "internal_tags")["source"] = "name" },
		"list mapping":         func(c gen.Context) { r := mappingRule(c, "internal_tags"); delete(r, "omit"); r["source"] = "name" },
		"parent child overlap": func(c gen.Context) {
			d := mappingDeclaration(c)
			d["fields"] = append(d["fields"].([]any), map[string]any{"target": "metadata", "omit": true})
		},
		"recursive message": func(c gen.Context) {
			c.Inputs["api.proto"] = []byte(shipmentProto + "message Tree{Tree child=1;}")
			mappingDeclaration(c)["message"] = "shipping.v1.Tree"
		},
		"unknown schema option": func(c gen.Context) { mappingDeclaration(c)["extra"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := shipmentContext(t)
			change(ctx)
			generator := new(grpcapplication.Generator)
			if err := generator.ValidateContext(ctx); err == nil {
				t.Fatal("invalid mapping passed preflight")
			}
			files, wiring, err := generator.Generate(ctx)
			if err == nil || len(files) != 0 || wiring != nil {
				t.Fatal("invalid mapping produced output", err)
			}
		})
	}
}

func TestResponseMappingOrderIsStable(t *testing.T) {
	ctx := shipmentContext(t)
	first, _, err := new(grpcapplication.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fields := mappingDeclaration(ctx)["fields"].([]any)
	for i, j := 0, len(fields)-1; i < j; i, j = i+1, j-1 {
		fields[i], fields[j] = fields[j], fields[i]
	}
	second, _, err := new(grpcapplication.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatal("file count changed")
	}
	for i := range first {
		if first[i].Path != second[i].Path || !bytes.Equal(first[i].Bytes(), second[i].Bytes()) {
			t.Fatal("mapping order changed generated output")
		}
	}
}

func TestGeneratedResponseMappings(t *testing.T) {
	for _, namespace := range []string{"grpcapi", "rpc/provider"} {
		t.Run(namespace, func(t *testing.T) {
			ctx := shipmentContext(t)
			ctx.OutputNamespace = namespace
			if err := new(grpcapplication.Generator).ValidateContext(ctx); err != nil {
				t.Fatal(err)
			}
			files, _, err := new(grpcapplication.Generator).Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			ctx.OutputNamespace = "store"
			ctx.ComponentConfig = nil
			storage, _, err := new(postgresadapter.Generator).Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range storage {
				if file.Path == "store/models.go" {
					files = append(files, file)
				}
			}
			project := t.TempDir()
			write := func(name string, data []byte) {
				t.Helper()
				name = filepath.Join(project, name)
				if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, data, 0644); err != nil {
					t.Fatal(err)
				}
			}
			for _, file := range files {
				if strings.HasPrefix(file.Path, namespace+"/pb/") || strings.HasPrefix(file.Path, namespace+"/mapping/") || file.Path == namespace+"/transport/conversion.go" || file.Path == "store/models.go" {
					write("out/"+file.Path, file.Bytes())
				}
			}
			data, err := os.ReadFile("testdata/mapping_test.go")
			if err != nil {
				t.Fatal(err)
			}
			write("out/"+namespace+"/mapping/mappings_test.go", []byte(strings.ReplaceAll(string(data), "MAPPING_NAMESPACE", namespace)))
			write("go.mod", []byte(fmt.Sprintf("module example.com/mapping-test\ngo %s\nrequire (\ngoogle.golang.org/protobuf v1.36.11\ngorm.io/gorm v1.25.12\n)\n", new(grpcapplication.Generator).MinimumGoVersion())))
			for _, args := range [][]string{{"mod", "tidy"}, {"vet", "-mod=readonly", "./..."}, {"test", "-json", "-race", "-count=1", "-mod=readonly", "-timeout=30s", "./out/" + namespace + "/mapping"}} {
				command := exec.Command("go", args...)
				command.Dir = project
				command.Env = append(os.Environ(), "GOWORK=off")
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("generated mapping: %v\n%s", err, output)
				}
				if args[0] == "test" {
					t.Logf("%s", output)
				}
			}
		})
	}
}

func TestResponseMappingJSONLimits(t *testing.T) {
	for name, change := range map[string]func(gen.Context){
		"combined byte budget": func(c gen.Context) {
			mappingRule(c, "tags")["max_bytes"] = 16 << 20
			replace := mappingRule(c, "internal_tags")
			for k := range replace {
				delete(replace, k)
			}
			for k, v := range mappingRule(c, "tags") {
				replace[k] = v
			}
			replace["target"] = "internal_tags"
		},
		"combined item budget": func(c gen.Context) {
			mappingRule(c, "tags")["max_items"] = 65536
			replace := mappingRule(c, "internal_tags")
			for k := range replace {
				delete(replace, k)
			}
			for k, v := range mappingRule(c, "tags") {
				replace[k] = v
			}
			replace["target"] = "internal_tags"
		},
		"missing bytes":      func(c gen.Context) { delete(mappingRule(c, "tags"), "max_bytes") },
		"missing items":      func(c gen.Context) { delete(mappingRule(c, "tags"), "max_items") },
		"missing item bytes": func(c gen.Context) { delete(mappingRule(c, "tags"), "max_item_bytes") },
		"zero bytes":         func(c gen.Context) { mappingRule(c, "tags")["max_bytes"] = 0 },
		"negative items":     func(c gen.Context) { mappingRule(c, "tags")["max_items"] = -1 },
		"zero item bytes":    func(c gen.Context) { mappingRule(c, "tags")["max_item_bytes"] = 0 },
		"byte ceiling":       func(c gen.Context) { mappingRule(c, "tags")["max_bytes"] = (16 << 20) + 1 },
		"item ceiling":       func(c gen.Context) { mappingRule(c, "tags")["max_items"] = 65537 },
		"item exceeds input": func(c gen.Context) { mappingRule(c, "tags")["max_item_bytes"] = 65 },
		"float limit":        func(c gen.Context) { mappingRule(c, "tags")["max_bytes"] = 64.0 },
		"string limit":       func(c gen.Context) { mappingRule(c, "tags")["max_bytes"] = "64" },
		"missing conversion": func(c gen.Context) { delete(mappingRule(c, "tags"), "conversion") },
		"wrong conversion":   func(c gen.Context) { mappingRule(c, "tags")["conversion"] = "int32" },
		"wrong source":       func(c gen.Context) { mappingRule(c, "tags")["source"] = "content" },
		"unknown source":     func(c gen.Context) { mappingRule(c, "tags")["source"] = "missing" },
		"prefix":             func(c gen.Context) { mappingRule(c, "tags")["prefix"] = "tag:" },
		"scalar limit":       func(c gen.Context) { mappingRule(c, "name")["max_bytes"] = 64 },
		"wrong list type": func(c gen.Context) {
			c.Inputs["api.proto"] = []byte(strings.Replace(shipmentProto, "repeated string tags", "repeated bytes tags", 1))
		},
		"scalar target": func(c gen.Context) {
			c.Inputs["api.proto"] = []byte(strings.Replace(shipmentProto, "repeated string tags", "string tags", 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := shipmentContext(t)
			change(ctx)
			g := new(grpcapplication.Generator)
			if g.ValidateContext(ctx) == nil {
				t.Fatal("invalid JSON declaration passed preflight")
			}
			files, wiring, err := g.Generate(ctx)
			if err == nil || len(files) != 0 || wiring != nil {
				t.Fatal("invalid JSON declaration produced output", err)
			}
		})
	}
}

func TestResponseMappingJSONHelperIsConditional(t *testing.T) {
	ctx := shipmentContext(t)
	rule := mappingRule(ctx, "tags")
	for key := range rule {
		if key != "target" {
			delete(rule, key)
		}
	}
	rule["omit"] = true
	files, _, err := new(grpcapplication.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file.Path, "/mapping/json_strings.go") {
			t.Fatal("unused JSON helper was generated")
		}
	}
}

func TestResponseMappingJSONBounds(t *testing.T) {
	ctx := shipmentContext(t)
	rule := mappingRule(ctx, "tags")
	rule["max_bytes"] = 16 << 20
	rule["max_items"] = 65536
	rule["max_item_bytes"] = 16 << 20
	g := new(grpcapplication.Generator)
	if err := g.ValidateContext(ctx); err != nil {
		t.Fatal("exact JSON declaration bound rejected", err)
	}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/mapping/json_strings.go") {
			found = true
		}
	}
	if !found {
		t.Fatal("JSON converter is missing")
	}
}

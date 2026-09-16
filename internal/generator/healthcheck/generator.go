// Package healthcheck generates bounded process and dependency probes.
package healthcheck

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"text/template"

	"github.com/jsell-rh/stego/internal/browserapplication"
	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed health.go.tmpl
var source string

//go:embed application.go.tmpl
var applicationSource string

type Generator struct{}

const livePattern, readyPattern = "GET /livez", "GET /readyz"

func (*Generator) HTTPRoutes(gen.Context) ([]gen.HTTPRoute, error) {
	return []gen.HTTPRoute{{Pattern: livePattern, Discovery: true}, {Pattern: readyPattern, Discovery: true}}, nil
}

func (*Generator) MinimumGoVersion() string { return "1.25.0" }
func (*Generator) ValidateContext(ctx gen.Context) error {
	if _, err := browserapplication.Resolve(ctx, ctx.PeerConfigs["kubernetes-service"]); err != nil {
		return err
	}
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return err
	}
	if ctx.ModuleName == "" {
		return fmt.Errorf("health-check requires a module name")
	}
	for key, value := range ctx.ComponentConfig {
		if key != "database" {
			return fmt.Errorf("unknown health-check setting %q", key)
		}
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("health-check database must be a boolean")
		}
	}
	return nil
}
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	tmpl, err := template.New("health").Parse(source)
	if err != nil {
		return nil, nil, err
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, struct{ Package string }{path.Base(ctx.OutputNamespace)}); err != nil {
		return nil, nil, err
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		return nil, nil, err
	}
	files := []gen.File{{Path: path.Join(ctx.OutputNamespace, "health.go"), Content: code}}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	constructor, name := "NewMonitor", "monitor"
	resources := []gen.Resource{gen.ServiceContext}
	database, _ := ctx.ComponentConfig["database"].(bool)
	if database {
		constructor, name = "NewDatabaseMonitor", "databaseMonitor"
		resources = append(resources, gen.SQLDatabase)
	}
	application, _ := browserapplication.Resolve(ctx, ctx.PeerConfigs["kubernetes-service"])
	if application != nil {
		template, err := template.New("application-health").Parse(applicationSource)
		if err != nil {
			return nil, nil, err
		}
		var output bytes.Buffer
		if err := template.Execute(&output, struct{ Package, Client, HealthPath string }{path.Base(ctx.OutputNamespace), path.Join(ctx.ModuleName, ctx.OutDirName, ctx.PeerNamespaces["browser-backend"], "client"), application.HealthPath}); err != nil {
			return nil, nil, err
		}
		content, err := format.Source(output.Bytes())
		if err != nil {
			return nil, nil, err
		}
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, "application.go"), Content: content})
		constructor, name = "NewApplicationMonitor", "applicationMonitor"
	}
	wiring := &gen.Wiring{
		Imports:                 []string{ctx.OutputNamespace},
		Constructors:            []string{path.Base(ctx.OutputNamespace) + "." + constructor + "()"},
		ConstructorResources:    map[int][]gen.Resource{0: resources},
		ConstructorReturnsError: map[int]bool{0: true},
		BackgroundTasks:         []int{0},
		NeedsDB:                 database,
		DiscoveryRoutes:         []string{fmt.Sprintf("topMux.HandleFunc(%q, %s.Live)", livePattern, name), fmt.Sprintf("topMux.HandleFunc(%q, %s.Ready)", readyPattern, name)},
	}
	if application != nil {
		wiring.ConstructorDeferCalls = map[int]string{0: "Close()"}
	}
	return files, wiring, gen.ValidateNamespace(ctx.OutputNamespace, files)
}

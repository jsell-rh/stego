package compiler

import (
	"reflect"
	"testing"

	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

func TestCommandFillCheck(t *testing.T) {
	t.Run("ST-SUP-RC32-OC03-T01 The fill qualifier gets each command fill with the command slot contract", func(t *testing.T) {
		svc := &types.ServiceDeclaration{Commands: []types.Command{
			{Name: "reserve-and-charge", Fill: types.FillNames{"reserve-and-charge"}},
			{Name: "two-fills", Fill: types.FillNames{"a", "b"}},
		}}
		components := map[string]*registry.Component{"command": {Name: "command", OutputNamespace: "commands"}}
		check := fillCheck(ReconcilerInput{ProjectDir: t.TempDir(), ModuleName: "example.com/orders"}, svc, nil, components)
		want := []CommandFill{{Fill: "reserve-and-charge", Command: "reserve-and-charge", Path: "commands[0].fill",
			Contract: "example.com/orders/out/commands"}}
		if !reflect.DeepEqual(check.CommandFills, want) {
			t.Errorf("the fill check holds the command fills %+v, want %+v", check.CommandFills, want)
		}
		if check := fillCheck(ReconcilerInput{ModuleName: "example.com/orders"}, svc, nil, map[string]*registry.Component{}); len(check.CommandFills) != 0 {
			t.Errorf("a service without the command component gives the command fills %+v", check.CommandFills)
		}
	})
}

func TestWorkerAndQueryFillCheck(t *testing.T) {
	t.Run("ST-SUP-RC35-OC03-T03 The fill qualifier gets each query fill and each worker fill", func(t *testing.T) {
		svc := &types.ServiceDeclaration{
			Queries: []types.Query{{Name: "open-orders", Fill: types.FillNames{"open-orders"}}},
			Workers: []types.Worker{{Name: "sync-orders", Fill: types.FillNames{"sync-orders"}}, {Name: "two", Fill: types.FillNames{"a", "b"}}},
		}
		components := map[string]*registry.Component{"query": {Name: "query", OutputNamespace: "queries"}, "worker": {Name: "worker", OutputNamespace: "workers"}}
		check := fillCheck(ReconcilerInput{ProjectDir: t.TempDir(), ModuleName: "example.com/orders"}, svc, nil, components)
		if want := []QueryFill{{Fill: "open-orders", Query: "open-orders", Path: "queries[0].fill"}}; !reflect.DeepEqual(check.QueryFills, want) {
			t.Errorf("the query fills are %+v, want %+v", check.QueryFills, want)
		}
		if want := []QueryFill{{Fill: "sync-orders", Query: "sync-orders", Path: "workers[0].fill"}}; !reflect.DeepEqual(check.WorkerFills, want) {
			t.Errorf("the worker fills are %+v, want %+v", check.WorkerFills, want)
		}
		if check.WorkerContract != "example.com/orders/out/workers" {
			t.Errorf("the worker contract is %q", check.WorkerContract)
		}
		if check := fillCheck(ReconcilerInput{ModuleName: "example.com/orders"}, svc, nil, map[string]*registry.Component{}); len(check.WorkerFills) != 1 || check.WorkerContract != "" {
			t.Errorf("with no worker component, the worker fills are %+v and the contract %q", check.WorkerFills, check.WorkerContract)
		}
	})
}

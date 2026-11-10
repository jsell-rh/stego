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

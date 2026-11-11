package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// ordersProto is the proto file of ST-DECL-INV73-T: the service orders.v1.OrderService with two unary methods and a
// server stream.
const ordersProto = `syntax = "proto3";

package orders.v1;

service OrderService {
  rpc ReserveAndCharge(ReserveAndChargeRequest) returns (ReserveAndChargeResponse);
  rpc ListOpenReservations(ListOpenReservationsRequest) returns (ListOpenReservationsResponse);
  rpc WatchOrders(WatchOrdersRequest) returns (stream Order);
}

message ReserveAndChargeRequest { string order_id = 1; }
message ReserveAndChargeResponse { string reservation_id = 1; }
message ListOpenReservationsRequest { string cursor = 1; }
message ListOpenReservationsResponse { repeated Order items = 1; string next = 2; }
message WatchOrdersRequest {}
message Order { string id = 1; }
`

func TestGRPCMethods(t *testing.T) {
	// The declaration of ST-DECL-TD02 with the command reserve-and-charge and the query open-reservations, each on
	// its gRPC method, checked in process as a value.
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "proto"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "proto", "orders.proto"), []byte(ordersProto), 0o644); err != nil {
		t.Fatal(err)
	}
	declared := func() *types.ServiceDeclaration {
		return &types.ServiceDeclaration{
			Name: "todo-svc", Archetype: "rest-crud", BasePath: "/api/todo/v1",
			Overrides: map[string]any{grpcComponent: map[string]any{"proto_files": []any{
				map[string]any{"path": "proto/orders.proto", "import_path": "orders/v1/orders.proto"}}}},
			Commands: []types.Command{{Name: "reserve-and-charge", Method: "orders.v1.OrderService/ReserveAndCharge"}},
			Queries:  []types.Query{{Name: "open-reservations", Method: "orders.v1.OrderService/ListOpenReservations"}},
		}
	}
	active := map[string]*registry.Component{grpcComponent: {Name: grpcComponent}}
	check := func(t *testing.T, svc *types.ServiceDeclaration, components map[string]*registry.Component) []declaration.ValidationError {
		t.Helper()
		methods, err := grpcMethodFacts(project, svc, components)
		if err != nil {
			t.Fatal(err)
		}
		var faults []declaration.ValidationError
		for _, f := range declaration.ValidateComposition(svc, declaration.Facts{GRPCMethods: methods}) {
			if f.Rule == "command-method" || f.Rule == "query-method" {
				faults = append(faults, f)
			}
		}
		return faults
	}
	refused := func(t *testing.T, faults []declaration.ValidationError, method, cause string) {
		t.Helper()
		if len(faults) != 1 || !strings.Contains(faults[0].Message, method) || faults[0].Found != method || !strings.Contains(faults[0].Message, cause) {
			t.Fatalf("the check gave %+v, want one fault that names the method %s and %q", faults, method, cause)
		}
	}

	t.Run("ST-DECL-INV73-T A gRPC method that is unknown, of the wrong kind or named twice is refused: the declared methods are accepted", func(t *testing.T) {
		if faults := check(t, declared(), active); len(faults) != 0 {
			t.Fatalf("the declaration with its declared methods gave %+v, want it accepted", faults)
		}
	})
	t.Run("ST-DECL-INV73-T A gRPC method that is unknown, of the wrong kind or named twice is refused: an unknown method", func(t *testing.T) {
		svc := declared()
		svc.Commands[0].Method = "orders.v1.OrderService/Cancel"
		refused(t, check(t, svc, active), "orders.v1.OrderService/Cancel", "no service of the proto files")
	})
	t.Run("ST-DECL-INV73-T A gRPC method that is unknown, of the wrong kind or named twice is refused: a method named twice", func(t *testing.T) {
		svc := declared()
		svc.Queries[0].Method = "orders.v1.OrderService/ReserveAndCharge"
		refused(t, check(t, svc, active), "orders.v1.OrderService/ReserveAndCharge", "already the method of command \"reserve-and-charge\"")
	})
	t.Run("ST-DECL-INV73-T A gRPC method that is unknown, of the wrong kind or named twice is refused: a command on a server stream", func(t *testing.T) {
		svc := declared()
		svc.Commands[0].Method = "orders.v1.OrderService/WatchOrders"
		refused(t, check(t, svc, active), "orders.v1.OrderService/WatchOrders", "server stream")
	})
	t.Run("ST-DECL-INV73-T A gRPC method that is unknown, of the wrong kind or named twice is refused: a query on a server stream is accepted and marked a stream", func(t *testing.T) {
		svc := declared()
		svc.Queries = append(svc.Queries, types.Query{Name: "open-reservations-stream", Method: "orders.v1.OrderService/WatchOrders"})
		if faults := check(t, svc, active); len(faults) != 0 {
			t.Fatalf("the query on the server stream gave %+v, want it accepted", faults)
		}
		methods, err := grpcMethodFacts(project, svc, active)
		if err != nil {
			t.Fatal(err)
		}
		streams := declaration.QueryStreams(svc, declaration.Facts{GRPCMethods: methods})
		if !streams["open-reservations-stream"] || streams["open-reservations"] || len(streams) != 1 {
			t.Fatalf("the stream queries are %v, want open-reservations-stream alone", streams)
		}
	})
	t.Run("ST-DECL-INV73-T A gRPC method that is unknown, of the wrong kind or named twice is refused: a method without the grpc-application component", func(t *testing.T) {
		svc := declared()
		svc.Queries = nil
		refused(t, check(t, svc, map[string]*registry.Component{}), "orders.v1.OrderService/ReserveAndCharge", "needs the grpc-application component")
	})
}

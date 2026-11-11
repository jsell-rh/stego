package query

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/internal/staging"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// metadataBind is the Bind of the RPC tests: the tenant and the subject come from two request metadata values, as the
// generated code takes them from the verified identity, and the probe call is bound to them and to the query name.
func metadataBind(q *Query) func(ctx context.Context) (Bound[noProbe], error) {
	return func(ctx context.Context) (Bound[noProbe], error) {
		md, _ := metadata.FromIncomingContext(ctx)
		values := md.Get("x-tenant")
		if len(values) != 1 {
			return Bound[noProbe]{}, errors.New("no tenant")
		}
		tn, err := tenant.New(tenant.Requested(tenant.ProvisionAction, values[0], decided{seal.NewDecisionMark(values[0], "provision")}))
		if err != nil {
			return Bound[noProbe]{}, err
		}
		p, err := authz.StagingPrincipal(staging.NewStaging(), md.Get("x-subject")[0])
		if err != nil {
			return Bound[noProbe]{}, err
		}
		call := probe.NewRecorder(nil).Bind(ctx, tn.ID(), p.Subject().String(), q.Name())
		return Bound[noProbe]{Tenant: tn, Principal: p, Call: call, Probe: newNoProbe}, nil
	}
}

// caller gives the incoming context of a call of tenantID and subject.
func caller(tenantID, subject string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-tenant", tenantID, "x-subject", subject))
}

// wantStatus checks the gRPC status of err: its code, and a message that holds none of the forbidden texts.
func wantStatus(t *testing.T, err error, code codes.Code, message string, forbidden ...string) {
	t.Helper()
	s, ok := status.FromError(err)
	if !ok || s.Code() != code {
		t.Fatalf("the call gave %v, want the status %s", err, code)
	}
	if message != "" && s.Message() != message {
		t.Fatalf("the status message is %q, want %q", s.Message(), message)
	}
	for _, text := range forbidden {
		if strings.Contains(s.Message(), text) {
			t.Fatalf("the status message %q holds %q", s.Message(), text)
		}
	}
}

func TestServeRPC(t *testing.T) {
	t.Run("ST-SUP-RP11-LC19-T01 One helper serves a get, a list and a stream over gRPC as Serve does over HTTP", func(t *testing.T) {
		e := newEnv(t)
		decision := &fakeDecision{decide: func(r authz.EvaluationRequest) bool { return keyOf(r.Resource) != "o2" }}
		rt := e.servedRuntime(t, decision)
		orders := orderEntity(t, false)
		charges := chargeEntity(t)
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', 'o1', 4711), ('t1', 'o2', 4712), ('t1', 'o3', 4713)")
		e.insert(t, "INSERT INTO app.charges (tenant_id, id, card_last4, amount) VALUES ('t1', 'ch-1', '4242', 100)")
		unknown := NewRefusal("get-order", "order_unknown", 404, "order.unknown")
		getOrder := mustQuery(t, "get-order", orders, charges)
		var fills atomic.Int64
		get, err := ServeRPC(rt, RPC[*wrapperspb.StringValue, *structpb.Struct, string, any, noProbe]{
			Query: getOrder,
			Bind:  metadataBind(getOrder),
			Decode: func(in *wrapperspb.StringValue, _ Cursor) (string, error) {
				if in.GetValue() == "" {
					return "", ErrBadRequest
				}
				return in.GetValue(), nil
			},
			Fill: func(ctx context.Context, id string, h *Handle[noProbe]) (any, error) {
				fills.Add(1)
				if id == "o-unknown" {
					return nil, unknown
				}
				if id == "o-fails" {
					return nil, errors.New("the fill failed with the secret 4242")
				}
				repo, err := Repo(h, orders)
				if err != nil {
					return nil, err
				}
				o, err := repo.Get(ctx, id)
				if err != nil {
					return nil, err
				}
				// The fill reads a charge with personal data, which writes a read audit row as over HTTP.
				chargeRepo, err := Repo(h, charges)
				if err != nil {
					return nil, err
				}
				if _, err := chargeRepo.Get(ctx, "ch-1"); err != nil {
					return nil, err
				}
				return map[string]any{"id": o.Value().ID, "total": o.Value().Total, "unknown_member": true}, nil
			},
			Encode:      EncodeValue[any],
			NewResponse: func() *structpb.Struct { return &structpb.Struct{} },
			Refusal:     Lookup(unknown),
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err := get(caller("t1", "user:u-7"), wrapperspb.String("o1"))
		if err != nil {
			t.Fatalf("the allowed get gave %v", err)
		}
		if body, _ := json.Marshal(out.AsMap()); string(body) != `{"id":"o1","total":4711,"unknown_member":true}` {
			t.Fatalf("the get gave %s, want the JSON body of the HTTP handler", body)
		}
		if n := e.count(t, "SELECT count(*) FROM "+audit.EventsTable+" WHERE action = 'read' AND operation = 'get-order' AND resource_id = 'ch-1' AND outcome = 'ok'"); n != 1 {
			t.Fatalf("the get wrote %d read audit rows of the charge, want one as over HTTP", n)
		}
		// A deny and a missing row are both NotFound, with no data of the row.
		_, err = get(caller("t1", "user:u-7"), wrapperspb.String("o2"))
		wantStatus(t, err, codes.NotFound, "not_found", "o2", "4712")
		_, err = get(caller("t1", "user:u-7"), wrapperspb.String("o9"))
		wantStatus(t, err, codes.NotFound, "not_found", "o9")
		before := fills.Load()
		_, err = get(caller("t1", "user:u-7"), wrapperspb.String(""))
		wantStatus(t, err, codes.InvalidArgument, "bad_request")
		_, err = get(context.Background(), wrapperspb.String("o1"))
		wantStatus(t, err, codes.Unauthenticated, "unauthenticated")
		if fills.Load() != before {
			t.Fatal("a bad request or a call with no identity reached the fill")
		}
		// A declared refusal answers its status as a gRPC code and its fixed code; any other error is Internal.
		_, err = get(caller("t1", "user:u-7"), wrapperspb.String("o-unknown"))
		wantStatus(t, err, codes.NotFound, "order.unknown")
		_, err = get(caller("t1", "user:u-7"), wrapperspb.String("o-fails"))
		wantStatus(t, err, codes.Internal, ErrorCodeInternal, "4242", "secret")

		// A list reads its cursor from the request message before any transaction, and its page is the JSON body of
		// HTTP read into the response message.
		lq := mustListQuery(t, "list-orders", orders, "view")
		var listFills atomic.Int64
		list, err := ServeRPC(rt, RPC[*structpb.Struct, *structpb.Struct, ListRequest, orderPage, noProbe]{
			Query:  lq,
			Bind:   metadataBind(lq),
			Cursor: func(in *structpb.Struct) string { return in.GetFields()["cursor"].GetStringValue() },
			Decode: func(_ *structpb.Struct, after Cursor) (ListRequest, error) {
				return ListRequest{Limit: 1, After: after}, nil
			},
			Fill: func(ctx context.Context, req ListRequest, h *Handle[noProbe]) (orderPage, error) {
				listFills.Add(1)
				repo, err := Repo(h, orders)
				if err != nil {
					return orderPage{}, err
				}
				page, err := repo.List(ctx, req)
				if err != nil {
					return orderPage{}, err
				}
				var p orderPage
				for _, o := range page.Items {
					p.IDs = append(p.IDs, o.Value().ID)
				}
				p.next = page.Next
				return p, nil
			},
			Next:        func(p orderPage) Cursor { return p.next },
			Encode:      func(p orderPage, next string) ([]byte, error) { return EncodePage(p.IDs, next) },
			NewResponse: func() *structpb.Struct { return &structpb.Struct{} },
		})
		if err != nil {
			t.Fatal(err)
		}
		var seen []string
		cursor := ""
		for i := 0; i < 5; i++ {
			in, _ := structpb.NewStruct(map[string]any{"cursor": cursor})
			page, err := list(caller("t1", "user:u-7"), in)
			if err != nil {
				t.Fatalf("the list page gave %v", err)
			}
			for _, item := range page.GetFields()["items"].GetListValue().GetValues() {
				seen = append(seen, item.GetStringValue())
			}
			cursor = page.GetFields()["next"].GetStringValue()
			if cursor == "" {
				break
			}
		}
		if strings.Join(seen, ",") != "o1,o3" {
			t.Fatalf("the list pages gave %v, want the allowed rows o1 and o3", seen)
		}
		calls := listFills.Load()
		bad, _ := structpb.NewStruct(map[string]any{"cursor": "garbage"})
		_, err = list(caller("t1", "user:u-7"), bad)
		wantStatus(t, err, codes.InvalidArgument, "bad_request")
		if listFills.Load() != calls {
			t.Fatal("the bad cursor reached the fill")
		}
		// A provider that fails is a read that could not be decided: Unavailable, not Internal.
		decision.mu.Lock()
		decision.down = true
		decision.mu.Unlock()
		_, err = list(caller("t1", "user:u-7"), &structpb.Struct{})
		wantStatus(t, err, codes.Unavailable, "undecided", "the provider is down")
		decision.mu.Lock()
		decision.down = false
		decision.mu.Unlock()
		// A list declaration needs its cursor key, its cursor and its next, and another query neither.
		noKey := e.runtimeOf(t, decision)
		if _, err := ServeRPC(noKey, RPC[*structpb.Struct, *structpb.Struct, ListRequest, orderPage, noProbe]{Query: lq,
			Bind: metadataBind(lq), Cursor: func(*structpb.Struct) string { return "" },
			Decode: func(*structpb.Struct, Cursor) (ListRequest, error) { return ListRequest{}, nil },
			Fill:   func(context.Context, ListRequest, *Handle[noProbe]) (orderPage, error) { return orderPage{}, nil },
			Next:   func(orderPage) Cursor { return Cursor{} }, Encode: func(orderPage, string) ([]byte, error) { return nil, nil },
			NewResponse: func() *structpb.Struct { return &structpb.Struct{} }}); !errors.Is(err, ErrNoCursorKey) {
			t.Fatalf("a list helper with no cursor key gave %v, want ErrNoCursorKey", err)
		}
		if _, err := ServeRPC(rt, RPC[*structpb.Struct, *structpb.Struct, ListRequest, orderPage, noProbe]{Query: lq,
			Bind:   metadataBind(lq),
			Decode: func(*structpb.Struct, Cursor) (ListRequest, error) { return ListRequest{}, nil },
			Fill:   func(context.Context, ListRequest, *Handle[noProbe]) (orderPage, error) { return orderPage{}, nil },
			Next:   func(orderPage) Cursor { return Cursor{} }, Encode: func(orderPage, string) ([]byte, error) { return nil, nil },
			NewResponse: func() *structpb.Struct { return &structpb.Struct{} }}); err == nil {
			t.Fatal("a list helper with Next and no Cursor was accepted")
		}

		// A stream sends each value that Send allows as one message, and the count of the dropped values reaches no
		// message and no status.
		sq := mustListQuery(t, "watch-orders", orders, "view")
		stream, err := ServeStream(StreamRPC[*wrapperspb.StringValue, *structpb.Struct, string, Order, noProbe]{
			Bind: metadataBind(sq),
			Decode: func(in *wrapperspb.StringValue) (string, error) {
				if in.GetValue() == "bad" {
					return "", ErrBadRequest
				}
				return in.GetValue(), nil
			},
			Stream: func(ctx context.Context, b Bound[noProbe], req string, send func(Order) error) (uint64, error) {
				return RunStream(ctx, rt, sq, b, req, orders, send, func(ctx context.Context, req string, s *Stream[Order, noProbe]) error {
					if req == "fail" {
						return errors.New("the stream fill failed with 4712")
					}
					repo, err := Repo(s.Handle(), orders)
					if err != nil {
						return err
					}
					page, err := repo.List(ctx, ListRequest{})
					if err != nil {
						return err
					}
					for _, o := range page.Items {
						if err := s.Send(ctx, o); err != nil {
							return err
						}
					}
					return nil
				})
			},
			NewMessage: func() *structpb.Struct { return &structpb.Struct{} },
		})
		if err != nil {
			t.Fatal(err)
		}
		var messages []string
		send := func(m *structpb.Struct) error {
			body, _ := json.Marshal(m.AsMap())
			messages = append(messages, string(body))
			return nil
		}
		if err := stream(caller("t1", "user:u-7"), wrapperspb.String(""), send); err != nil {
			t.Fatalf("the stream gave %v", err)
		}
		if strings.Join(messages, ",") != `{"ID":"o1","Total":4711},{"ID":"o3","Total":4713}` {
			t.Fatalf("the stream sent %v, want o1 and o3 as the JSON form of the values", messages)
		}
		messages = nil
		wantStatus(t, stream(caller("t1", "user:u-7"), wrapperspb.String("bad"), send), codes.InvalidArgument, "bad_request")
		wantStatus(t, stream(context.Background(), wrapperspb.String(""), send), codes.Unauthenticated, "unauthenticated")
		wantStatus(t, stream(caller("t1", "user:u-7"), wrapperspb.String("fail"), send), codes.Internal, ErrorCodeInternal, "4712")
		if len(messages) != 0 {
			t.Fatalf("a refused stream sent %v", messages)
		}
		// A send that the transport refuses ends the stream with the transport's status.
		gone := status.Error(codes.Canceled, "the client went away")
		wantStatus(t, stream(caller("t1", "user:u-7"), wrapperspb.String(""), func(*structpb.Struct) error { return gone }), codes.Canceled, "")
		if _, err := ServeStream(StreamRPC[*wrapperspb.StringValue, *structpb.Struct, string, Order, noProbe]{}); err == nil {
			t.Fatal("a stream helper with no parts was accepted")
		}
	})
}

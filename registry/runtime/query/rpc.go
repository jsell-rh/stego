package query

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/probe"
)

// RequestIDMetadata is the gRPC request metadata whose value is the request ID of the audit rows of a query.
const RequestIDMetadata = "x-request-id"

// RPC is what the generated code gives ServeRPC for one query that a unary gRPC method serves: only what varies with
// the declaration and with the method's messages (ST-SUP-RP11-LC19, ST-STD-RUNTIME-R05). Fill, Next, Encode and
// Refusal are the values of the query's HTTP handler, so both transports run the same query.
type RPC[In any, Out proto.Message, Req, Res, P any] struct {
	// Query is the query declaration.
	Query *Query
	// Bind gives the verified tenant and principal of the call, its probe call and its request ID, as BindRPC
	// gives them. An error is answered Unauthenticated.
	Bind func(ctx context.Context) (Bound[P], error)
	// Cursor gives the text form of the list cursor that the request message holds, empty for the start of the
	// list. It is nil for a query that is not a list.
	Cursor func(in In) string
	// Decode reads the request of the query from the request message; after is the cursor that Cursor gave, the
	// zero cursor when there is none. An error is answered InvalidArgument.
	Decode func(in In, after Cursor) (Req, error)
	// Fill is the query fill, as the HTTP handler calls it.
	Fill Fill[Req, Res, P]
	// Next gives the cursor of the next page of a list response, or the zero cursor at the end. It is nil for a
	// query that is not a list.
	Next func(res Res) Cursor
	// Encode writes the response as the JSON body of the HTTP handler: res, and next, the text form of the next
	// cursor, empty at the end.
	Encode func(res Res, next string) ([]byte, error)
	// NewResponse gives an empty response message, into which ServeRPC reads the JSON body with protojson.
	NewResponse func() Out
	// Refusal is the refusal lookup of the query, as for HTTP. Nil knows no refusal.
	Refusal func(err error) (status int, code string, ok bool)
}

// ServeRPC returns the unary gRPC handler of the query that s declares (ST-SUP-RP11-LC19): the steps of Serve with
// the request message in place of the HTTP request. It binds the verified identity of the call's context, which the
// transport put there with authn.Authenticate, reads the cursor of a list with ParseCursor before any transaction,
// decodes the request, runs the query with Run, and reads the JSON body that Encode writes into a new response
// message with protojson, which drops a member that the message does not declare. It answers a failed bind
// Unauthenticated, a bad cursor or request InvalidArgument, a row that is not there or is denied NotFound, a read
// that could not be decided Unavailable, a declared refusal with the gRPC code of its status, and every other error
// Internal. The message of each error status is a fixed code or text: no error text, no data of a row. It refuses a
// list declaration when the runtime has no cursor key.
func ServeRPC[In any, Out proto.Message, Req, Res, P any](rt *Runtime, s RPC[In, Out, Req, Res, P]) (func(context.Context, In) (Out, error), error) {
	switch {
	case rt == nil:
		return nil, errors.New("query: no runtime")
	case s.Query == nil || s.Bind == nil || s.Decode == nil || s.Fill == nil || s.Encode == nil || s.NewResponse == nil:
		return nil, errors.New("query: ServeRPC needs the query, Bind, Decode, Fill, Encode and NewResponse")
	case (s.Next == nil) != (s.Cursor == nil):
		return nil, errors.New("query: a list needs both Cursor and Next, and another query neither")
	case s.Next != nil && rt.cursorAEAD == nil:
		return nil, ErrNoCursorKey
	}
	return func(ctx context.Context, in In) (Out, error) {
		var none Out
		b, err := s.Bind(ctx)
		if err != nil {
			return none, status.Error(codes.Unauthenticated, "unauthenticated")
		}
		// The probe call ends here when no Run takes it.
		ran := false
		defer func() {
			if !ran && b.Call != nil {
				b.Call.Returned()
				b.Call.Rollback()
			}
		}()
		var after Cursor
		if s.Cursor != nil {
			if after, err = rt.ParseCursor(s.Query, b.Tenant, b.Principal, s.Cursor(in)); err != nil {
				return none, status.Error(codes.InvalidArgument, "bad_request")
			}
		}
		req, err := s.Decode(in, after)
		if err != nil {
			return none, status.Error(codes.InvalidArgument, "bad_request")
		}
		ran = true
		res, err := Run(ctx, rt, s.Query, b, req, s.Fill)
		if err != nil {
			return none, rpcError(ctx, err, s.Refusal)
		}
		next := ""
		if s.Next != nil {
			if next, err = rt.CursorText(s.Query, b.Tenant, b.Principal, s.Next(res)); err != nil {
				return none, internal(ctx)
			}
		}
		body, err := s.Encode(res, next)
		if err != nil {
			return none, internal(ctx)
		}
		out := s.NewResponse()
		if err := decodeMessage(body, out); err != nil {
			return none, internal(ctx)
		}
		return out, nil
	}, nil
}

// StreamRPC is what the generated code gives ServeStream for one query that a server stream serves
// (ST-SUP-RP11-LC19): only what varies with the declaration and with the method's messages.
type StreamRPC[In any, Out proto.Message, Req, T, P any] struct {
	// Bind gives the verified tenant and principal of the stream, as BindRPC gives them. An error is answered
	// Unauthenticated.
	Bind func(ctx context.Context) (Bound[P], error)
	// Decode reads the request of the query from the request message. An error is answered InvalidArgument.
	Decode func(in In) (Req, error)
	// Stream runs the query with its fill through RunStream, as the generated Stream function of the query does.
	Stream func(ctx context.Context, b Bound[P], req Req, send func(T) error) (uint64, error)
	// NewMessage gives an empty stream message, into which ServeStream reads the JSON form of each sent value.
	NewMessage func() Out
	// Refusal is the refusal lookup of the query. Nil knows no refusal.
	Refusal func(err error) (status int, code string, ok bool)
}

// ServeStream returns the server stream handler of the query that s declares (ST-SUP-RP11-LC19): it binds the
// verified identity of the stream's context, decodes the request, and runs Stream once, which sends each value that
// Send allows, as the JSON form of the value read into a new stream message with protojson, with send. The stream
// ends when the fill returns or the context ends, such as at the stream lifetime or at the token's expiry that the
// transport sets. It answers its errors as ServeRPC does. The count of the values that a deny dropped never reaches
// the client.
func ServeStream[In any, Out proto.Message, Req, T, P any](s StreamRPC[In, Out, Req, T, P]) (func(ctx context.Context, in In, send func(Out) error) error, error) {
	if s.Bind == nil || s.Decode == nil || s.Stream == nil || s.NewMessage == nil {
		return nil, errors.New("query: ServeStream needs Bind, Decode, Stream and NewMessage")
	}
	return func(ctx context.Context, in In, send func(Out) error) error {
		if send == nil {
			return internal(ctx)
		}
		b, err := s.Bind(ctx)
		if err != nil {
			return status.Error(codes.Unauthenticated, "unauthenticated")
		}
		req, err := s.Decode(in)
		if err != nil {
			if b.Call != nil {
				b.Call.Returned()
				b.Call.Rollback()
			}
			return status.Error(codes.InvalidArgument, "bad_request")
		}
		var sendErr error
		_, err = s.Stream(ctx, b, req, func(value T) error {
			body, err := json.Marshal(value)
			if err != nil {
				return err
			}
			message := s.NewMessage()
			if err := decodeMessage(body, message); err != nil {
				return err
			}
			if err := send(message); err != nil {
				// A failed send is the transport's: the client went away or the stream ended. It ends the fill.
				sendErr = err
				return err
			}
			return nil
		})
		if err != nil {
			if sendErr != nil && errors.Is(err, sendErr) {
				if code := status.Code(sendErr); code != codes.Unknown {
					return status.Error(code, code.String())
				}
				return status.FromContextError(ctx.Err()).Err()
			}
			if ctx.Err() != nil {
				return status.FromContextError(ctx.Err()).Err()
			}
			return rpcError(ctx, err, s.Refusal)
		}
		return nil
	}, nil
}

// BindRPC gives the Bind of RPC and StreamRPC for the query q: BindContext of the call's context, with the request ID
// of the RequestIDMetadata request metadata.
func BindRPC[P any](probes Probes, subjectType string, q *Query, newProbe func(*probe.Probe) P) func(ctx context.Context) (Bound[P], error) {
	return func(ctx context.Context) (Bound[P], error) {
		requestID := ""
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if values := md.Get(RequestIDMetadata); len(values) == 1 {
				requestID = values[0]
			}
		}
		return BindContext(ctx, probes, subjectType, q, newProbe, requestID)
	}
}

// decodeMessage reads the JSON body into the message with protojson; a member that the message does not declare is
// dropped.
func decodeMessage(body []byte, out proto.Message) error {
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(body, out)
}

// internal is the Internal status of a query that failed with an error that is not a declared refusal, whose code it
// gives the completion log.
func internal(ctx context.Context) error {
	otel.SetErrorCode(ctx, ErrorCodeInternal)
	return status.Error(codes.Internal, ErrorCodeInternal)
}

// rpcError maps an error of Run or RunStream to its gRPC status, as Serve maps it to an HTTP status: a declared
// refusal to the code of its status with its fixed code as the message, and the completion log gets the fixed code.
func rpcError(ctx context.Context, err error, refusal func(error) (int, string, bool)) error {
	httpStatus, code := statusOf(err), ""
	if refusal != nil {
		if declared, declaredCode, ok := refusal(err); ok && declared >= 400 && declared <= 599 {
			httpStatus, code = declared, declaredCode
		}
	}
	if code == "" && httpStatus == http.StatusInternalServerError {
		code = ErrorCodeInternal
	}
	otel.SetErrorCode(ctx, code)
	message := code
	if message == "" {
		message = rpcMessages[httpStatus]
	}
	return status.Error(rpcCode(httpStatus), message)
}

// rpcMessages are the fixed messages of the statuses that carry no code.
var rpcMessages = map[int]string{
	http.StatusBadRequest:         "bad_request",
	http.StatusNotFound:           "not_found",
	http.StatusServiceUnavailable: "undecided",
}

// rpcCode is the gRPC status code of an HTTP status of a query answer.
func rpcCode(httpStatus int) codes.Code {
	switch httpStatus {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusConflict:
		return codes.Aborted
	case http.StatusPreconditionFailed:
		return codes.FailedPrecondition
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	}
	return codes.Internal
}

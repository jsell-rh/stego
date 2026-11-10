package query

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// MaxRequestBody is the most bytes of a request body that Serve lets Decode read.
const MaxRequestBody = 1 << 20

// CursorParameter is the URL query parameter that holds the text form of a list cursor (ST-SUP-RP11-LC15).
const CursorParameter = "cursor"

// ErrBadRequest is the error that a generated Decode wraps for a request that does not decode or is not valid. The
// helper answers it with 400.
var ErrBadRequest = errors.New("query: the request is not valid")

// HTTP is what the generated code gives Serve for one query: only what varies with the declaration (ST-SUP-RP11-LC17,
// ST-STD-RUNTIME-R05).
type HTTP[Req, Res, P any] struct {
	// Query is the query declaration.
	Query *Query
	// Bind gives the verified tenant and principal of the request, the probe call bound to them and to the query
	// name, the typed probe constructor and the request ID. An error is answered with 401.
	Bind func(r *http.Request) (Bound[P], error)
	// Decode reads the request of the query from the HTTP request; after is the cursor of the cursor parameter, the
	// zero cursor when there is none. An error is answered with 400.
	Decode func(r *http.Request, after Cursor) (Req, error)
	// Fill is the query fill.
	Fill Fill[Req, Res, P]
	// Next gives the cursor of the next page of a list response, or the zero cursor at the end. It is nil for a
	// query that is not a list.
	Next func(res Res) Cursor
	// Encode writes the response body: res, and next, the text form of the next cursor, empty at the end.
	Encode func(res Res, next string) ([]byte, error)
}

// problem is a problem response (RFC 9457). It holds a fixed title and the status only: no error text and no data
// of a row.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
}

// The fixed titles of the problem responses.
var titles = map[int]string{
	http.StatusBadRequest:          "the request is not valid",
	http.StatusUnauthorized:        "the request is not authenticated",
	http.StatusNotFound:            "the resource is not found",
	http.StatusMethodNotAllowed:    "the method is not allowed",
	http.StatusInternalServerError: "the query failed",
	http.StatusServiceUnavailable:  "the read could not be decided",
}

// Serve returns the HTTP handler of the query that s declares (ST-SUP-RP11-LC17): for each GET request it binds the
// verified identity, reads the cursor parameter with ParseCursor, before any transaction and so with no row read,
// decodes the request, whose body it limits to MaxRequestBody, runs the query with Run, and writes the response as
// JSON with 200 and Cache-Control: no-store, as it does each problem response. It maps each refusal to a
// problem response: a bad cursor or request to 400, a failed bind to 401, a row that is not there or is denied to
// 404, a read that could not be decided, also a list whose provider fails, to 503, and every other error to 500. A problem response holds no error
// text and no data of a row. It refuses a list declaration when the runtime has no cursor key.
func Serve[Req, Res, P any](rt *Runtime, s HTTP[Req, Res, P]) (http.Handler, error) {
	switch {
	case rt == nil:
		return nil, errors.New("query: no runtime")
	case s.Query == nil || s.Bind == nil || s.Decode == nil || s.Fill == nil || s.Encode == nil:
		return nil, errors.New("query: Serve needs the query, Bind, Decode, Fill and Encode")
	case s.Next != nil && rt.cursorAEAD == nil:
		return nil, ErrNoCursorKey
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A query response holds data of one principal: no cache keeps it.
		w.Header().Set("Cache-Control", "no-store")
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBody)
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeProblem(w, http.StatusMethodNotAllowed)
			return
		}
		b, err := s.Bind(r)
		if err != nil {
			writeProblem(w, http.StatusUnauthorized)
			return
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
		if s.Next != nil {
			if after, err = rt.ParseCursor(s.Query, b.Tenant, b.Principal, r.URL.Query().Get(CursorParameter)); err != nil {
				writeProblem(w, http.StatusBadRequest)
				return
			}
		}
		req, err := s.Decode(r, after)
		if err != nil {
			writeProblem(w, http.StatusBadRequest)
			return
		}
		ran = true
		res, err := Run(r.Context(), rt, s.Query, b, req, s.Fill)
		if err != nil {
			writeProblem(w, statusOf(err))
			return
		}
		next := ""
		if s.Next != nil {
			if next, err = rt.CursorText(s.Query, b.Tenant, b.Principal, s.Next(res)); err != nil {
				writeProblem(w, http.StatusInternalServerError)
				return
			}
		}
		body, err := s.Encode(res, next)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}), nil
}

// statusOf is the status of an error of Run.
func statusOf(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrBadCursor), errors.Is(err, ErrBadRequest):
		return http.StatusBadRequest
	case errors.Is(err, ErrUndecided):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// writeProblem writes the problem response of status.
func writeProblem(w http.ResponseWriter, status int) {
	body, _ := json.Marshal(problem{Type: "about:blank", Title: titles[status], Status: status})
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

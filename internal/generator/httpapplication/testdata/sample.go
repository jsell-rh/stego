package sample

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"

	"example.com/http-test/out/application/transport"
	"example.com/http-test/out/auth"
	storage "example.com/http-test/out/contracts/storage"
)

type Repository interface {
	storage.Storage
	storage.Transactor
}
type Details struct {
	Label string `json:"label"`
}
type Request struct {
	Title   string   `json:"title"`
	Details *Details `json:"details,omitempty"`
}
type Response struct {
	Title   string `json:"title"`
	Subject string `json:"subject"`
}

var Calls atomic.Int64

func New(repository Repository, verifier *auth.Verifier, db *sql.DB) (http.Handler, error) {
	return transport.Endpoint(verifier.Authenticate, transport.JSONBody[Request], func(ctx context.Context, request Request) (Response, error) {
		Calls.Add(1)
		if request.Title == "failure" {
			return Response{}, errors.New("private database error")
		}
		return Response{Title: request.Title, Subject: auth.IdentityFromContext(ctx).UserID}, nil
	}, http.StatusCreated, func(w http.ResponseWriter, r *http.Request, err error) {
		code := 500
		if errors.Is(err, transport.ErrRequest) {
			code = 400
		}
		if errors.Is(err, transport.ErrUnauthenticated) {
			code = 401
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			code = 503
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]int{"status": code})
	})
}

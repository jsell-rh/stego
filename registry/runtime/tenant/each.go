package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Func is the work that Each runs for one tenant, inside the transaction that Begin opened for that tenant.
type Func func(ctx context.Context, tx *sql.Tx, t Tenant) error

// Failure is the failure of one tenant in a run of Each.
type Failure struct {
	Tenant Tenant
	Err    error
}

// EachError is the error of a run of Each in which the function failed for one tenant or more. It names every
// failed tenant, in the order Each visited them.
type EachError struct {
	Failures []Failure
}

func (e *EachError) Error() string {
	parts := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		parts = append(parts, fmt.Sprintf("%s: %v", f.Tenant.id, f.Err))
	}
	return fmt.Sprintf("tenant: %d tenants failed: %s", len(e.Failures), strings.Join(parts, "; "))
}

// Unwrap returns the error of each failed tenant, for errors.Is and errors.As.
func (e *EachError) Unwrap() []error {
	errs := make([]error, 0, len(e.Failures))
	for _, f := range e.Failures {
		errs = append(errs, f.Err)
	}
	return errs
}

// Each runs fn once for each tenant of tenants, in their order, each in its own transaction that Begin opens with
// that tenant set, and commits the transaction when fn returns nil (ST-SUP-RP04-LC05). No transaction serves two
// tenants. When fn or the commit fails for one tenant, Each rolls that transaction back and goes on with the next
// tenant, and at the end returns an *EachError that names every failed tenant. It refuses a zero tenant in tenants
// before it sends a statement. A canceled ctx stops the run before the next tenant, and the tenants left are
// reported as failed with the error of ctx.
func Each(ctx context.Context, db *sql.DB, tenants []Tenant, fn Func) error {
	for _, t := range tenants {
		if t.IsZero() {
			return ErrZero
		}
	}
	if db == nil {
		return errors.New("tenant: no database")
	}
	if fn == nil {
		return errors.New("tenant: no function to run")
	}
	var failures []Failure
	for _, t := range tenants {
		if err := ctx.Err(); err != nil {
			failures = append(failures, Failure{Tenant: t, Err: err})
			continue
		}
		if err := runOne(ctx, db, t, fn); err != nil {
			failures = append(failures, Failure{Tenant: t, Err: err})
		}
	}
	if len(failures) > 0 {
		return &EachError{Failures: failures}
	}
	return nil
}

func runOne(ctx context.Context, db *sql.DB, t Tenant, fn Func) error {
	tx, err := Begin(ctx, db, t, nil)
	if err != nil {
		return err
	}
	if err := fn(ctx, tx, t); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("tenant: commit the transaction of tenant %s: %w", t.id, err)
	}
	return nil
}

// EachListed reads the tenant list with List and runs Each over it: the iterator of a worker tick or a controller
// pass, which visits every provisioned tenant that is not retired, in tenant ID order (ST-SUP-RP04-LC07).
func EachListed(ctx context.Context, db *sql.DB, fn Func) error {
	tenants, err := List(ctx, db)
	if err != nil {
		return err
	}
	return Each(ctx, db, tenants, fn)
}

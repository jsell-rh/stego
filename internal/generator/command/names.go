package command

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/types"
)

// The grammars of the declared names that the generated code holds.
var (
	operationName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	fillName      = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	refusalName   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	refusalCode   = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,62}$`)
	identifier    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	relationText  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}#[a-z][a-z0-9_]{0,62}$`)
	sqlName       = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	effectName    = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)
)

// refusalStatus holds the statuses that a declared refusal may have (ST-DECL-INV56).
var refusalStatus = map[int]bool{400: true, 403: true, 404: true, 409: true, 412: true, 422: true}

// mixedCase gives the exported Go name of a lower-case name with hyphens or underscores, such as ReserveAndCharge
// for reserve-and-charge and OrderID for order_id.
func mixedCase(name string) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		if part == "id" {
			b.WriteString("ID")
			continue
		}
		runes := []rune(part)
		runes[0] = unicode.ToUpper(runes[0])
		b.WriteString(string(runes))
	}
	return b.String()
}

// tableName is the table of an entity, as the PostgreSQL adapter creates it (ST-SUP-RC01-OC41).
func tableName(entity string) string { return postgresadapter.TableName(entity) }

// retryAndLock reads the command's Retry on conflict and Lock timeout (ST-DECL-EN18): the retries from 0 to 3, and
// the lock timeout as a Go duration expression, empty for the runtime's default.
func retryAndLock(decl types.Command) (int, string, error) {
	retry := 0
	if decl.RetryOnConflict != "" {
		n, err := strconv.Atoi(decl.RetryOnConflict)
		if err != nil || n < 0 || n > 3 {
			return 0, "", fmt.Errorf("command: the command %s has the retry on conflict %q; it must be a whole number from 0 to 3", decl.Name, decl.RetryOnConflict)
		}
		retry = n
	}
	lock := ""
	if decl.LockTimeout != "" {
		d, err := time.ParseDuration(decl.LockTimeout)
		if err != nil || d < 100*time.Millisecond || d > 10*time.Second {
			return 0, "", fmt.Errorf("command: the command %s has the lock timeout %q; it must be a duration from 100ms to 10s", decl.Name, decl.LockTimeout)
		}
		lock = fmt.Sprintf("%d * time.Millisecond", d.Milliseconds())
	}
	return retry, lock, nil
}

package declaration

import "fmt"

// The shape bounds of a .zed schema (ST-DECL-INV88). The SpiceDB schema compiler takes seconds on a wide permission
// expression of some hundred kilobytes and minutes near MaxSchemaBytes, so checking reads the shape of the source
// first, in one pass, and refuses a schema beyond a bound before it compiles it (orchestrator decision of
// 2026-11-14 under the Product owner's delegation).
const (
	// MaxPermissionTerms is the most terms that one permission expression may have, where a term is a relation, an
	// arrow such as parent->view, a function such as parent.any(view), or nil.
	MaxPermissionTerms = 256
	// MaxSchemaMembers is the most relations and permissions that a schema may have in all its definitions.
	MaxSchemaMembers = 4096
)

// checkSchemaShape reads the source of a .zed schema once, without compiling it, and refuses it with a *SchemaError
// that names the limit when a permission expression has more than MaxPermissionTerms terms or the schema has more
// than MaxSchemaMembers relations and permissions. It counts the terms of an expression as its operators (+, & and
// -) and one. Every operator outside a comment and a caveat body counts toward the statement it follows, at any
// depth, so no text that the compiler reads can hide one; a caveat body, a CEL expression that the caveat compiler
// bounds, is not counted. A source that is no schema, such as one with a syntax error, it leaves to the compiler.
func checkSchemaShape(schemaPath string, source []byte) error {
	var (
		line, members, terms int
		statement            string // the relation or permission whose statement is read, or empty
		statementLine        int
		expectName           bool
		caveat               bool // a caveat statement is read: its body is the next brace at its depth
		body                 int  // the brace depth inside a caveat body, or 0
		previous             string
	)
	line = 1
	for i := 0; i < len(source); {
		c := source[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == '/' && i+1 < len(source) && source[i+1] == '/':
			for i < len(source) && source[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(source) && source[i+1] == '*':
			for i += 2; i < len(source) && (source[i] != '*' || i+1 >= len(source) || source[i+1] != '/'); i++ {
				if source[i] == '\n' {
					line++
				}
			}
			i += 2
		case body > 0:
			// A caveat body: CEL, whose strings may hold a brace.
			switch c {
			case '"', '\'':
				for i++; i < len(source) && source[i] != c && source[i] != '\n'; i++ {
					if source[i] == '\\' {
						i++
					}
				}
			case '{':
				body++
			case '}':
				body--
			}
			i++
		case isNameByte(c):
			start := i
			for i < len(source) && (isNameByte(source[i]) || source[i] == '/') {
				i++
			}
			word := string(source[start:i])
			switch {
			case expectName:
				expectName = false
				statement = word
			case word == "relation" || word == "permission":
				members++
				if members > MaxSchemaMembers {
					return &SchemaError{Path: schemaPath, Line: line, Message: fmt.Sprintf("the schema has more than %d relations and permissions, the most that checking compiles", MaxSchemaMembers)}
				}
				statement, statementLine, terms, expectName = word, line, 1, true
			case word == "caveat" && previous != "definition":
				caveat, statement, terms = true, "", 1
			case word == "definition" || word == "use":
				statement, terms = "", 1
			}
			previous = word
		default:
			i++
			switch c {
			case '{':
				if caveat {
					caveat, body = false, 1
				}
			case '+', '&', '-':
				if c == '-' && i < len(source) && source[i] == '>' {
					i++
					continue
				}
				if terms++; terms > MaxPermissionTerms {
					name := "expression"
					if statement != "" {
						name = statement
					}
					return &SchemaError{Path: schemaPath, Line: statementLine, Message: fmt.Sprintf("the permission %s has more than %d terms in its expression, the most that checking compiles", name, MaxPermissionTerms)}
				}
			}
		}
	}
	return nil
}

// isNameByte tells whether c can be in a name of the schema language.
func isNameByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

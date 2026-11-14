package declaration

import (
	"bytes"
	"fmt"

	"github.com/authzed/spicedb/pkg/schemadsl/input"
	"github.com/authzed/spicedb/pkg/schemadsl/lexer"
)

// The shape bounds of a .zed schema (ST-DECL-INV88). The SpiceDB schema compiler takes seconds on a wide permission
// expression of some hundred kilobytes and minutes near MaxSchemaBytes, and its parser recurses once for each
// parenthesis, so a deep nesting ends the process with a stack overflow. Checking reads the shape of the source
// first, in one pass of the lexer of the compiler, and refuses a schema beyond a bound before it compiles it
// (orchestrator decisions of 2026-11-14 under the Product owner's delegation).
const (
	// MaxPermissionTerms is the most terms that one permission expression may have, where a term is a relation, an
	// arrow such as parent->view, a function such as parent.any(view), or nil.
	MaxPermissionTerms = 256
	// MaxSchemaMembers is the most relations and permissions that a schema may have in all its definitions.
	MaxSchemaMembers = 4096
	// MaxSchemaNesting is the most parentheses that may be open at one place of a schema.
	MaxSchemaNesting = 64
)

// checkSchemaShape reads the source of a .zed schema once with the lexer of the SpiceDB schema compiler, without
// compiling it, and refuses it with a *SchemaError that names the limit when a permission expression has more than
// MaxPermissionTerms terms, the schema has more than MaxSchemaMembers relations and permissions, or more than
// MaxSchemaNesting parentheses are open at one place. Because it reads the tokens that the parser reads, a string or
// a comment is one token and hides no operator, and the end of a caveat body is the brace that the parser finds. It
// counts the terms of an expression as its operators (+, & and -) and one; every operator outside a caveat body
// counts toward the statement it follows, at any depth. A caveat body, a CEL expression that the caveat compiler
// bounds, is not counted for terms. At a lexer error it stops: the compiler reads no further than that token.
func checkSchemaShape(schemaPath string, source []byte) error {
	lx := lexer.NewFlaggableLexer(lexer.Lex(input.Source(schemaPath), string(source)))
	defer lx.Close()
	var (
		line, members, terms, nesting int
		seen                          int    // the byte offset up to which line counts the newlines
		statement                     string // the relation or permission whose statement is read, or empty
		statementLine                 int
		expectName                    bool
		caveat                        bool // a top-level caveat statement is read: its body is the next brace
		depth                         int  // the brace depth outside a caveat body
		body                          int  // the brace depth inside a caveat body, or 0
		previous                      lexer.Lexeme
	)
	line = 1
	fault := func(at int, format string, args ...any) error {
		return &SchemaError{Path: schemaPath, Line: at, Message: fmt.Sprintf(format, args...)}
	}
	for {
		token := lx.NextToken()
		switch token.Kind {
		case lexer.TokenTypeEOF, lexer.TokenTypeError:
			return nil
		case lexer.TokenTypeWhitespace, lexer.TokenTypeNewline, lexer.TokenTypeSyntheticSemicolon,
			lexer.TokenTypeSinglelineComment, lexer.TokenTypeMultilineComment:
			continue
		}
		if at := int(token.Position); at > seen && at <= len(source) {
			line += bytes.Count(source[seen:at], []byte{'\n'})
			seen = at
		}
		switch token.Kind {
		case lexer.TokenTypeLeftParen:
			if nesting++; nesting > MaxSchemaNesting {
				return fault(line, "the schema has more than %d parentheses open at one place, the most that checking compiles", MaxSchemaNesting)
			}
		case lexer.TokenTypeRightParen:
			nesting--
		}
		if body > 0 {
			switch token.Kind {
			case lexer.TokenTypeLeftBrace:
				body++
			case lexer.TokenTypeRightBrace:
				body--
			}
			previous = token
			continue
		}
		switch token.Kind {
		case lexer.TokenTypeLeftBrace:
			if caveat {
				caveat, body = false, 1
			} else {
				depth++
			}
		case lexer.TokenTypeRightBrace:
			depth--
		case lexer.TokenTypeIdentifier, lexer.TokenTypeKeyword:
			word := token.Value
			switch {
			case expectName:
				expectName = false
				statement = word
			case token.Kind == lexer.TokenTypeKeyword && (word == "relation" || word == "permission"):
				members++
				if members > MaxSchemaMembers {
					return fault(line, "the schema has more than %d relations and permissions, the most that checking compiles", MaxSchemaMembers)
				}
				statement, statementLine, terms, expectName = word, line, 1, true
			case token.Kind == lexer.TokenTypeKeyword && word == "caveat" && depth == 0 &&
				!(previous.Kind == lexer.TokenTypeKeyword && previous.Value == "definition"):
				caveat, statement, terms = true, "", 1
			case token.Kind == lexer.TokenTypeKeyword && word == "definition", word == "use":
				statement, terms = "", 1
			}
		case lexer.TokenTypePlus, lexer.TokenTypeAnd, lexer.TokenTypeMinus:
			if terms++; terms > MaxPermissionTerms {
				name := "expression"
				if statement != "" {
					name = statement
				}
				return fault(statementLine, "the permission %s has more than %d terms in its expression, the most that checking compiles", name, MaxPermissionTerms)
			}
		}
		previous = token
	}
}

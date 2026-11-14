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
	// MaxCaveatTypeNesting is the most type arguments that may be open at one place of the parameter types of a
	// caveat, such as the two of list<list<int>>. The CEL checker formats a nested type in a time that grows with
	// the square of its depth (review of PR #302, M3).
	MaxCaveatTypeNesting = 8
	// MaxCaveats is the most caveats that a schema may have.
	MaxCaveats = 64
	// MaxCaveatBodyBytes is the most bytes that the bodies of all caveats of a schema may have, the text between the
	// braces of each body. The CEL compiler takes about 0.9 s for each caveat body of 95 KB (review of PR #302, L2).
	MaxCaveatBodyBytes = 64 << 10
)

// checkSchemaShape reads the source of a .zed schema once with the lexer of the SpiceDB schema compiler, without
// compiling it, and refuses it with a *SchemaError that names the limit when a permission expression has more than
// MaxPermissionTerms terms, the schema has more than MaxSchemaMembers relations and permissions, or more than
// MaxSchemaNesting parentheses are open at one place, more than MaxCaveatTypeNesting type arguments are open at one
// place of the parameter types of a caveat, the schema has more than MaxCaveats caveats, or the caveat bodies have
// more than MaxCaveatBodyBytes bytes in all. Because it reads the tokens that the parser reads, a string or
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
		caveats, types                int  // the caveats read, and the type arguments open in the caveat parameters
		bodyStart, bodyBytes          int  // the offset after the brace of the body read, and the bytes of the bodies before it
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
			if body > 0 && bodyBytes+len(source)-bodyStart > MaxCaveatBodyBytes {
				return fault(line, "the caveat bodies have more than %d bytes, the most that checking compiles", MaxCaveatBodyBytes)
			}
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
				if body--; body == 0 {
					if bodyBytes += int(token.Position) - bodyStart; bodyBytes > MaxCaveatBodyBytes {
						return fault(line, "the caveat bodies have more than %d bytes, the most that checking compiles", MaxCaveatBodyBytes)
					}
				}
			}
			previous = token
			continue
		}
		switch token.Kind {
		case lexer.TokenTypeLeftBrace:
			if caveat {
				caveat, body, bodyStart = false, 1, int(token.Position)+1
			} else {
				depth++
			}
		case lexer.TokenTypeRightBrace:
			depth--
		case lexer.TokenTypeLessThan:
			if !caveat {
				break
			}
			if types++; types > MaxCaveatTypeNesting {
				return fault(line, "a caveat parameter type has more than %d type arguments open at one place, the most that checking compiles", MaxCaveatTypeNesting)
			}
		case lexer.TokenTypeGreaterThan:
			if caveat && types > 0 {
				types--
			}
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
				if caveats++; caveats > MaxCaveats {
					return fault(line, "the schema has more than %d caveats, the most that checking compiles", MaxCaveats)
				}
				caveat, statement, terms, types = true, "", 1, 0
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

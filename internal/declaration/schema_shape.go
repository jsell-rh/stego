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
	// MaxCaveatMacros is the most comprehension macros (map, filter, all, exists and exists_one) that the caveat
	// bodies of a schema may have in all. Each macro can wrap the type that the CEL checker infers once more, as in
	// [1].map(x, [x]).map(y, [y]), and the checker formats a nested type in a time that grows with the square of its
	// depth (review of PR #302, M4).
	MaxCaveatMacros = 16
	// MaxCaveatBodyNesting is the most brackets, braces and parentheses that may be open together at one place of a
	// caveat body, such as the three of [{1: (n)}], for the same reason: each list literal, map literal and call can
	// wrap the inferred type once more, so the depth of the type is at most their sum (review of PR #302, M4 and M5).
	MaxCaveatBodyNesting = 24
	// MaxCaveatBodyBytesEach is the most bytes that one caveat body may have, and MaxCaveatBodyCalls the most calls,
	// indexes and operators that it may have. The CEL checker takes a time that grows with the square of the
	// overloaded calls of one expression: 4000 of m['a'] != m['b'] in one body of 64 KB took 53 s, and the same bytes
	// over 16 caveats less than 4 s (review of PR #302, M6).
	MaxCaveatBodyBytesEach = 4 << 10
	MaxCaveatBodyCalls     = 256
)

// caveatOperators are the tokens of a caveat body that are a CEL operator, each a call of an overload.
var caveatOperators = map[lexer.TokenType]bool{
	lexer.TokenTypePlus: true, lexer.TokenTypeMinus: true, lexer.TokenTypeStar: true, lexer.TokenTypeDiv: true,
	lexer.TokenTypePercent: true, lexer.TokenTypeEqualEqual: true, lexer.TokenTypeNotEqual: true,
	lexer.TokenTypeLessThan: true, lexer.TokenTypeGreaterThan: true, lexer.TokenTypeLessThanOrEqual: true,
	lexer.TokenTypeGreaterThanOrEqual: true, lexer.TokenTypeConditionalAnd: true, lexer.TokenTypeConditionalOr: true,
	lexer.TokenTypeExclamationPoint: true, lexer.TokenTypeQuestionMark: true, lexer.TokenTypeAnd: true, lexer.TokenTypePipe: true,
}

// postfix tells whether a token can end an operand, so that a ( or a [ after it is a call or an index.
func postfix(token lexer.Lexeme) bool {
	switch token.Kind {
	case lexer.TokenTypeIdentifier, lexer.TokenTypeKeyword, lexer.TokenTypeRightParen, lexer.TokenTypeRightBracket,
		lexer.TokenTypeRightBrace, lexer.TokenTypeString, lexer.TokenTypeNumber:
		return true
	}
	return false
}

// caveatMacros are the CEL comprehension macros, which a caveat body calls on a receiver, as in xs.map(x, x + 1).
var caveatMacros = map[string]bool{"map": true, "filter": true, "all": true, "exists": true, "exists_one": true}

// checkSchemaShape reads the source of a .zed schema once with the lexer of the SpiceDB schema compiler, without
// compiling it, and refuses it with a *SchemaError that names the limit when a permission expression has more than
// MaxPermissionTerms terms, the schema has more than MaxSchemaMembers relations and permissions, or more than
// MaxSchemaNesting parentheses are open at one place, more than MaxCaveatTypeNesting type arguments are open at one
// place of the parameter types of a caveat, the schema has more than MaxCaveats caveats, or the caveat bodies have
// more than MaxCaveatBodyBytes bytes in all, more than MaxCaveatMacros comprehension macros in all, or more than
// MaxCaveatBodyNesting brackets, braces and parentheses open together at one place of a caveat body, or a caveat
// body has more than MaxCaveatBodyBytesEach bytes or more than MaxCaveatBodyCalls calls, indexes and operators (a
// ( or [ after an operand, a token of caveatOperators, or the word in). A macro counts where a name of caveatMacros follows a period
// and comes before a parenthesis. Because it reads the tokens that the parser reads, a string or
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
		macros, open                  int  // the comprehension macros of the caveat bodies, and the [, { and ( open in the body read
		macro                         bool // the token before is the name of a macro after a period, in a caveat body
		calls                         int  // the calls, indexes and operators of the body read
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
			if body > 0 && len(source)-bodyStart > MaxCaveatBodyBytesEach {
				return fault(line, "a caveat body has more than %d bytes, the most that checking compiles", MaxCaveatBodyBytesEach)
			}
			if body > 0 && bodyBytes+len(source)-bodyStart > MaxCaveatBodyBytes {
				return fault(line, "the caveat bodies have more than %d bytes, the most that checking compiles", MaxCaveatBodyBytes)
			}
			return nil
		}
		// The lexer gives each space as a token of its own, so a body beyond its bytes is refused at the first token
		// past the bound, a space or a comment too, and not only at its closing brace (review of PR #302, M6).
		if body > 0 && int(token.Position)-bodyStart > MaxCaveatBodyBytesEach {
			if at := int(token.Position); at > seen && at <= len(source) {
				line += bytes.Count(source[seen:at], []byte{'\n'})
				seen = at
			}
			return fault(line, "a caveat body has more than %d bytes, the most that checking compiles", MaxCaveatBodyBytesEach)
		}
		switch token.Kind {
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
			if macro && token.Kind == lexer.TokenTypeLeftParen {
				if macros++; macros > MaxCaveatMacros {
					return fault(line, "the caveat bodies have more than %d comprehension macros, the most that checking compiles", MaxCaveatMacros)
				}
			}
			if caveatOperators[token.Kind] || token.Kind == lexer.TokenTypeIdentifier && token.Value == "in" ||
				(token.Kind == lexer.TokenTypeLeftParen || token.Kind == lexer.TokenTypeLeftBracket) && postfix(previous) {
				if calls++; calls > MaxCaveatBodyCalls {
					return fault(line, "a caveat body has more than %d calls, indexes and operators, the most that checking compiles", MaxCaveatBodyCalls)
				}
			}
			macro = (token.Kind == lexer.TokenTypeIdentifier || token.Kind == lexer.TokenTypeKeyword) && caveatMacros[token.Value] &&
				previous.Kind == lexer.TokenTypePeriod
			switch token.Kind {
			case lexer.TokenTypeLeftBracket, lexer.TokenTypeLeftBrace, lexer.TokenTypeLeftParen:
				if open++; open > MaxCaveatBodyNesting {
					return fault(line, "a caveat body has more than %d brackets, braces and parentheses open at one place, the most that checking compiles", MaxCaveatBodyNesting)
				}
			case lexer.TokenTypeRightBracket, lexer.TokenTypeRightParen:
				if open > 0 {
					open--
				}
			}
			switch token.Kind {
			case lexer.TokenTypeLeftBrace:
				body++
			case lexer.TokenTypeRightBrace:
				if open > 0 && body > 1 {
					open--
				}
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
				caveat, body, bodyStart, open, macro, calls = false, 1, int(token.Position)+1, 0, false, 0
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

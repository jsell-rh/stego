// Package slot provides a minimal protobuf parser and Go interface generator
// for stego slot definitions. It extracts service and message definitions from
// .proto files and generates Go interfaces that fills implement.
//
// This is intentionally not a full protobuf parser — it handles the subset of
// proto3 used by stego slot and common type definitions.
package slot

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// ProtoFile represents a parsed .proto file.
type ProtoFile struct {
	Syntax   string
	Package  string
	Imports  []string
	Services []Service
	Messages []Message
}

// Service represents a protobuf service definition.
type Service struct {
	Name    string
	Methods []Method
}

// Method represents a single RPC method in a service.
type Method struct {
	Name       string
	InputType  string
	OutputType string
}

// MessageField represents a single field in a protobuf message.
type MessageField struct {
	Type   string
	Name   string
	Number int
}

// Message represents a protobuf message definition.
type Message struct {
	Name   string
	Fields []MessageField
}

// ParseProto parses a .proto file from the given reader and returns a ProtoFile.
// It handles the subset of proto3 used by stego: syntax, package, import,
// service/rpc, and message definitions.
func ParseProto(r io.Reader) (*ProtoFile, error) {
	pf := &ProtoFile{}
	scanner := bufio.NewScanner(r)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments.
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}

		switch {
		case strings.HasPrefix(line, "syntax"):
			val, err := parseQuotedValue(line, "syntax")
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNum, err)
			}
			pf.Syntax = val

		case strings.HasPrefix(line, "package"):
			val, err := parseUnquotedValue(line, "package")
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNum, err)
			}
			pf.Package = val

		case strings.HasPrefix(line, "import"):
			val, err := parseQuotedValue(line, "import")
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNum, err)
			}
			pf.Imports = append(pf.Imports, val)

		case strings.HasPrefix(line, "service"):
			svc, err := parseService(line, scanner, &lineNum)
			if err != nil {
				return nil, err
			}
			pf.Services = append(pf.Services, svc)

		case strings.HasPrefix(line, "message"):
			msg, err := parseMessage(line, scanner, &lineNum)
			if err != nil {
				return nil, err
			}
			pf.Messages = append(pf.Messages, msg)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading proto: %w", err)
	}

	if pf.Syntax == "" {
		return nil, fmt.Errorf("missing syntax declaration")
	}
	if pf.Package == "" {
		return nil, fmt.Errorf("missing package declaration")
	}

	return pf, nil
}

// parseQuotedValue extracts a quoted string value from a line like:
//
//	syntax = "proto3";
func parseQuotedValue(line, keyword string) (string, error) {
	rest := strings.TrimPrefix(line, keyword)
	rest = strings.TrimSpace(rest)
	rest = strings.TrimPrefix(rest, "=")
	rest = strings.TrimSpace(rest)
	rest = strings.TrimSuffix(rest, ";")
	rest = strings.TrimSpace(rest)
	if len(rest) < 2 || rest[0] != '"' || rest[len(rest)-1] != '"' {
		return "", fmt.Errorf("expected quoted value for %s", keyword)
	}
	return rest[1 : len(rest)-1], nil
}

// parseUnquotedValue extracts an unquoted value from a line like:
//
//	package stego.common;
func parseUnquotedValue(line, keyword string) (string, error) {
	rest := strings.TrimPrefix(line, keyword)
	rest = strings.TrimSpace(rest)
	rest = strings.TrimSuffix(rest, ";")
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", fmt.Errorf("expected value for %s", keyword)
	}
	return rest, nil
}

// parseService parses a service block like:
//
//	service BeforeCreate {
//	  rpc Evaluate(...) returns (...);
//	}
func parseService(firstLine string, scanner *bufio.Scanner, lineNum *int) (Service, error) {
	name := extractBlockName(firstLine, "service")
	if name == "" {
		return Service{}, fmt.Errorf("line %d: missing service name", *lineNum)
	}

	svc := Service{Name: name}

	for scanner.Scan() {
		*lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if line == "}" {
			return svc, nil
		}
		if strings.HasPrefix(line, "rpc") {
			m, err := parseRPC(line)
			if err != nil {
				return Service{}, fmt.Errorf("line %d: %w", *lineNum, err)
			}
			svc.Methods = append(svc.Methods, m)
		}
	}

	return Service{}, fmt.Errorf("unexpected end of input in service %s", name)
}

// parseRPC parses an rpc line like:
//
//	rpc Evaluate(BeforeCreateRequest) returns (stego.common.SlotResult);
func parseRPC(line string) (Method, error) {
	line = strings.TrimPrefix(line, "rpc")
	line = strings.TrimSpace(line)
	line = strings.TrimSuffix(line, ";")
	line = strings.TrimSpace(line)

	// Extract method name.
	parenIdx := strings.Index(line, "(")
	if parenIdx < 0 {
		return Method{}, fmt.Errorf("malformed rpc: missing '('")
	}
	methodName := strings.TrimSpace(line[:parenIdx])

	// Extract input type.
	closeIdx := strings.Index(line, ")")
	if closeIdx < 0 {
		return Method{}, fmt.Errorf("malformed rpc: missing ')' after input type")
	}
	inputType := strings.TrimSpace(line[parenIdx+1 : closeIdx])

	// Find "returns".
	rest := strings.TrimSpace(line[closeIdx+1:])
	if !strings.HasPrefix(rest, "returns") {
		return Method{}, fmt.Errorf("malformed rpc: expected 'returns'")
	}
	rest = strings.TrimPrefix(rest, "returns")
	rest = strings.TrimSpace(rest)

	// Extract output type.
	openIdx := strings.Index(rest, "(")
	closeIdx2 := strings.Index(rest, ")")
	if openIdx < 0 || closeIdx2 < 0 {
		return Method{}, fmt.Errorf("malformed rpc: missing parens around return type")
	}
	outputType := strings.TrimSpace(rest[openIdx+1 : closeIdx2])

	return Method{
		Name:       methodName,
		InputType:  inputType,
		OutputType: outputType,
	}, nil
}

// parseMessage parses a message block like:
//
//	message BeforeCreateRequest {
//	  stego.common.CreateRequest input = 1;
//	  stego.common.Identity caller = 2;
//	}
func parseMessage(firstLine string, scanner *bufio.Scanner, lineNum *int) (Message, error) {
	name := extractBlockName(firstLine, "message")
	if name == "" {
		return Message{}, fmt.Errorf("line %d: missing message name", *lineNum)
	}

	msg := Message{Name: name}

	for scanner.Scan() {
		*lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if line == "}" {
			return msg, nil
		}

		field, err := parseField(line)
		if err != nil {
			return Message{}, fmt.Errorf("line %d: %w", *lineNum, err)
		}
		msg.Fields = append(msg.Fields, field)
	}

	return Message{}, fmt.Errorf("unexpected end of input in message %s", name)
}

// parseField parses a message field line like:
//
//	stego.common.CreateRequest input = 1;
//	string entity = 2;
//	map<string, string> attributes = 3;
func parseField(line string) (MessageField, error) {
	line = strings.TrimSuffix(line, ";")
	line = strings.TrimSpace(line)

	// Handle map<K,V> types specially since they contain spaces.
	var fieldType, rest string
	if strings.HasPrefix(line, "map<") {
		closeAngle := strings.Index(line, ">")
		if closeAngle < 0 {
			return MessageField{}, fmt.Errorf("malformed map type in field: %s", line)
		}
		fieldType = line[:closeAngle+1]
		rest = strings.TrimSpace(line[closeAngle+1:])
	} else {
		parts := strings.Fields(line)
		if len(parts) < 4 {
			return MessageField{}, fmt.Errorf("malformed field: %s", line)
		}
		fieldType = parts[0]
		rest = strings.Join(parts[1:], " ")
	}

	// rest is now "name = number"
	eqIdx := strings.Index(rest, "=")
	if eqIdx < 0 {
		return MessageField{}, fmt.Errorf("malformed field (missing '='): %s", line)
	}

	name := strings.TrimSpace(rest[:eqIdx])
	numStr := strings.TrimSpace(rest[eqIdx+1:])

	var num int
	if _, err := fmt.Sscanf(numStr, "%d", &num); err != nil {
		return MessageField{}, fmt.Errorf("malformed field number %q: %w", numStr, err)
	}

	return MessageField{
		Type:   fieldType,
		Name:   name,
		Number: num,
	}, nil
}

// extractBlockName extracts the name from a block-opening line like
// "service BeforeCreate {" or "message Identity {".
func extractBlockName(line, keyword string) string {
	rest := strings.TrimPrefix(line, keyword)
	rest = strings.TrimSpace(rest)
	rest = strings.TrimSuffix(rest, "{")
	rest = strings.TrimSpace(rest)
	return rest
}

package browserassets

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// ScriptHashes permits only the exact inline scripts in the captured document.
// Input HTML is application code, not untrusted user content. These checks
// diagnose unsupported constructs; they do not make an HTML sanitizer.
func ScriptHashes(data []byte, names map[string]bool) ([]string, error) {
	if len(data) > MaxFile || !utf8.Valid(data) {
		return nil, fmt.Errorf("invalid browser HTML")
	}
	tokenizer := html.NewTokenizer(bytes.NewReader(data))
	tokenizer.SetMaxBuf(MaxFile)
	hashes := map[string]bool{}
	inScript := false
	external := false
	var script strings.Builder
	count := 0
	for {
		kind := tokenizer.Next()
		count++
		if count > 10000 {
			return nil, fmt.Errorf("browser HTML exceeds its node limit")
		}
		if kind == html.ErrorToken {
			if tokenizer.Err() != io.EOF || inScript {
				return nil, fmt.Errorf("invalid browser HTML")
			}
			break
		}
		token := tokenizer.Token()
		if kind == html.StartTagToken || kind == html.SelfClosingTagToken {
			if token.Data == "style" || token.Data == "base" || token.Data == "iframe" || token.Data == "object" || token.Data == "embed" || token.Data == "svg" || token.Data == "math" || token.Data == "noscript" || token.Data == "template" {
				return nil, fmt.Errorf("unsupported active browser HTML element")
			}
			attrs := map[string]string{}
			for _, attr := range token.Attr {
				if _, exists := attrs[attr.Key]; exists || strings.HasPrefix(attr.Key, "on") || attr.Key == "style" || attr.Key == "nonce" || attr.Namespace != "" {
					return nil, fmt.Errorf("unsupported browser HTML attribute")
				}
				attrs[attr.Key] = attr.Val
			}
			if token.Data == "script" {
				if inScript || kind == html.SelfClosingTagToken {
					return nil, fmt.Errorf("invalid browser script element")
				}
				inScript = true
				script.Reset()
				external = false
				if typ := attrs["type"]; typ != "" && typ != "module" && typ != "text/javascript" {
					return nil, fmt.Errorf("unsupported browser script type")
				}
				if src, present := attrs["src"]; present {
					if !strings.HasPrefix(src, "/") || !names[strings.TrimPrefix(src, "/")] || path.Ext(src) != ".js" {
						return nil, fmt.Errorf("browser script must use a captured asset")
					}
					external = true
				}
			}
			if token.Data == "link" && executableLink(attrs["rel"]) {
				href := attrs["href"]
				if !strings.HasPrefix(href, "/") || !names[strings.TrimPrefix(href, "/")] {
					return nil, fmt.Errorf("browser link must use a captured asset")
				}
			}
		}
		if inScript && kind == html.TextToken {
			script.WriteString(token.Data)
		}
		if kind == html.EndTagToken && token.Data == "script" {
			if !inScript {
				return nil, fmt.Errorf("invalid browser script close")
			}
			inScript = false
			content := strings.ReplaceAll(strings.ReplaceAll(script.String(), "\r\n", "\n"), "\r", "\n")
			if external && strings.TrimSpace(content) != "" {
				return nil, fmt.Errorf("external browser script must have no inline content")
			}
			if !external && content != "" {
				sum := sha256.Sum256([]byte(content))
				hashes["'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'"] = true
				if len(hashes) > 32 {
					return nil, fmt.Errorf("browser HTML has too many inline scripts")
				}
			}
		}
	}
	result := make([]string, 0, len(hashes))
	for hash := range hashes {
		result = append(result, hash)
	}
	sort.Strings(result)
	return result, nil
}

func executableLink(rel string) bool {
	for _, value := range strings.Fields(strings.ToLower(rel)) {
		if value == "stylesheet" || value == "modulepreload" || value == "preload" {
			return true
		}
	}
	return false
}

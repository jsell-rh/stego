package authn

import (
	"context"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// discoveryURL is the URL of the discovery document of the issuer: the issuer with no trailing slash and
// /.well-known/openid-configuration (OpenID Connect Discovery 1.0, section 4).
func discoveryURL(issuer string) string {
	return strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
}

// discover loads the discovery document of the issuer once and gives its jwks_uri. It refuses a document that it
// cannot load, that is larger than MaxDocumentBytes, that the JSON rules refuse, whose issuer member is not exactly
// the issuer or whose jwks_uri member is not an HTTPS URL. Each refusal names the discovery document and holds no
// URL or provider text, and the span holds the outcome only (ST-SUP-RP13-LC12, LC21).
func (s *KeySet) discover(parent context.Context, client *http.Client, issuer string) (string, error) {
	ctx, cancel := context.WithTimeout(s.stop, LoadTimeout)
	defer cancel()
	defer context.AfterFunc(parent, cancel)()
	ctx = trace.ContextWithSpanContext(ctx, trace.SpanContextFromContext(parent))
	ctx, span := s.tracer.Start(ctx, "authn.keys.discover", trace.WithSpanKind(trace.SpanKindClient))
	location, err := readDiscovery(ctx, client, issuer)
	outcome := "success"
	if err != nil {
		outcome = "failure"
		span.SetStatus(codes.Error, "")
	}
	span.SetAttributes(attribute.String("authn.keys.outcome", outcome))
	span.End()
	return location, err
}

func readDiscovery(ctx context.Context, client *http.Client, issuer string) (string, error) {
	data, err := fetch(ctx, client, discoveryURL(issuer))
	if err != nil || checkJSON(data) != nil {
		return "", errDiscovery
	}
	document, err := members(data)
	if err != nil {
		return "", errDiscovery
	}
	if named, ok := stringMember(document, "issuer"); !ok || named != issuer {
		return "", errDiscoveryIssuer
	}
	location, ok := stringMember(document, "jwks_uri")
	if !ok || !httpsURL(location) {
		return "", errDiscoveryKeys
	}
	return location, nil
}

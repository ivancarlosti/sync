package microsoft

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"

	"github.com/ivancarlosti/sync/internal/providers"
)

// Compile time guard: the accounts screen asks every provider for the kind of an
// account, so a renamed method must fail the build.
var _ providers.AccountClassifier = (*Provider)(nil)

// consumerTenantID is the tenant Microsoft accounts (MSAs) live in. A personal
// account is not a member of a directory: it is a tenant of its own, and the
// tenant id is the well known *consumers* tenant Microsoft documents — every
// token issued to an MSA carries it in the `tid` claim
// (https://learn.microsoft.com/en-us/entra/identity-platform/id-token-claims-reference).
// It is the one signal that tells a personal account from a work or school one
// without asking Graph, which is what a list of stored accounts needs.
const consumerTenantID = "9188040d-6c67-4c5b-b112-36a304b66dad"

// AccountKindOf implements providers.AccountClassifier: it names the kind of the
// account a token set belongs to from the access token itself.
//
// The claim is read, never verified, and that is deliberate: the answer only
// decides which capability badges the Accounts screen greys out instead of
// showing red (see permissions.go), while every real operation still goes through
// Graph with the token it was handed. A token that is opaque, empty or not a JWT
// answers unknown, which leaves every permission applicable.
func (p *Provider) AccountKindOf(tokens *providers.Tokens) providers.AccountKind {
	if tokens == nil {
		return providers.AccountKindUnknown
	}
	switch tenant := tokenTenant(tokens.AccessToken); {
	case tenant == "":
		return providers.AccountKindUnknown
	case strings.EqualFold(tenant, consumerTenantID):
		return providers.AccountKindPersonal
	default:
		return providers.AccountKindWork
	}
}

// accountKind names the account behind tokens during a connection (OAuth
// completion, token reissue, identity refresh). The tenant claim answers it for
// free; a token whose claims cannot be read falls back to the account's own
// drive — the probe SearchSites already uses to recognise a personal account —
// and answers unknown when even that fails, which keeps every permission
// applicable, exactly as it was before the kind existed.
//
// client must be bound to tokens, and the failure of the probe is deliberately
// swallowed: a kind is a display hint, so it must never fail a connection.
func (p *Provider) accountKind(ctx context.Context, client *msgraphsdk.GraphServiceClient, tokens *providers.Tokens) providers.AccountKind {
	if kind := p.AccountKindOf(tokens); kind != providers.AccountKindUnknown {
		return kind
	}
	own, err := p.ownDrive(ctx, client)
	if err != nil {
		return providers.AccountKindUnknown
	}
	if isPersonalDrive(own) {
		return providers.AccountKindPersonal
	}
	return providers.AccountKindWork
}

// tokenTenant reads the tenant of a JWT access token: the `tid` claim, or the
// tenant segment of the `iss` claim when the platform left `tid` out (the v1
// issuer is `https://sts.windows.net/<tenant>/`, the v2 one is
// `https://login.microsoftonline.com/<tenant>/v2.0`). The payload of a JWT is
// base64url without padding; anything that does not decode as a JSON object
// answers "" rather than guessing.
func tokenTenant(accessToken string) string {
	payload, err := jwtPayload(accessToken)
	if err != nil {
		return ""
	}
	var claims struct {
		Tenant string `json:"tid"`
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	if tenant := strings.TrimSpace(claims.Tenant); tenant != "" {
		return tenant
	}
	return issuerTenant(claims.Issuer)
}

// jwtPayload decodes the claims of a compact JWS/JWT: the second of the three
// dot-separated segments, base64url without padding.
func jwtPayload(token string) ([]byte, error) {
	segments := strings.Split(token, ".")
	if len(segments) < 2 || segments[1] == "" {
		return nil, errNoJWTPayload
	}
	payload, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		// A few issuers pad their base64url; try the padded alphabet before
		// giving up on a token that is otherwise readable.
		if padded, paddedErr := base64.URLEncoding.DecodeString(segments[1]); paddedErr == nil {
			return padded, nil
		}
		return nil, err
	}
	return payload, nil
}

// issuerTenant extracts the tenant segment of an issuer URL, i.e. the first
// non-empty path segment. An issuer that does not look like an URL answers "".
func issuerTenant(issuer string) string {
	issuer = strings.TrimSpace(issuer)
	index := strings.Index(issuer, "//")
	if index < 0 {
		return ""
	}
	rest := issuer[index+2:]
	index = strings.Index(rest, "/")
	if index < 0 {
		return ""
	}
	rest = strings.Trim(rest[index+1:], "/")
	if index = strings.Index(rest, "/"); index >= 0 {
		rest = rest[:index]
	}
	return rest
}

// errNoJWTPayload is returned for a token that carries no readable claims.
var errNoJWTPayload = errors.New("microsoft graph: the token carries no JWT payload")

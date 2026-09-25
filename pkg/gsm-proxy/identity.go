package gsmproxy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/api/idtoken"
)

const RedHatDomain = "redhat.com"

// GcloudOAuthClientID is the "aud" claim on an identity token from "gcloud auth print-identity-token".
//
// It is gcloud's own OAuth client, shared by every installation on earth, so this value excludes
// nobody: it is not the control that keeps an attacker out. The signature check, hd=redhat.com,
// email_verified, and rover group membership are. It is asserted only so a token minted for some
// other Google client cannot be replayed here.
//
// A user account cannot pick its own audience -- "gcloud auth print-identity-token --audiences=..."
// fails with "Invalid account type for `--audiences`. Requires valid service account." -- so this is
// the only value the proxy can accept today. Note it is not the ADC client: "gcloud auth
// application-default login" writes 764086051850-6qr4p6gpi6hn506pt8ejuq83di341hur.apps.googleusercontent.com,
// so it matters which of the two the CLI mints its token from.
//
// Google documents the value only incidentally, as a literal in the Apps Script sample that revokes
// gcloud access org-wide:
// https://docs.cloud.google.com/docs/security/compromised-credentials
const GcloudOAuthClientID = "32555940559.apps.googleusercontent.com"

// googleIssuers are the two spellings Google uses for the "iss" claim.
var googleIssuers = map[string]bool{
	"accounts.google.com":         true,
	"https://accounts.google.com": true,
}

// kerberosPattern is the shape of a Red Hat kerberos id, which is what groups.yaml is keyed by.
var kerberosPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

var ErrUnauthenticated = errors.New("unauthenticated")

// TokenValidator is the subset of the idtoken package the verifier needs, extracted so tests can
// supply payloads without minting Google-signed tokens.
type TokenValidator interface {
	Validate(ctx context.Context, rawToken, audience string) (*idtoken.Payload, error)
}

// GoogleTokenValidator validates against Google's published signing keys.
type GoogleTokenValidator struct{}

func (GoogleTokenValidator) Validate(ctx context.Context, rawToken, audience string) (*idtoken.Payload, error) {
	return idtoken.Validate(ctx, rawToken, audience)
}

// GoogleIdentityVerifier validates a Google-issued identity token.
//
// idtoken.Validate checks the signature against Google's published keys, rejects any algorithm
// other than RS256 and ES256, checks the expiry, and compares the audience. It does not check the
// issuer, the hosted domain, or whether the email is verified, so this type checks those itself
// rather than relying on the library's internals staying as they are.
type GoogleIdentityVerifier struct {
	Audience  string
	Validator TokenValidator
}

func (v *GoogleIdentityVerifier) Verify(ctx context.Context, rawToken string) (string, error) {
	if v.Audience == "" {
		return "", errors.New("verifier has no audience configured")
	}
	payload, err := v.Validator.Validate(ctx, rawToken, v.Audience)
	if err != nil {
		return "", fmt.Errorf("%w: token rejected: %v", ErrUnauthenticated, err)
	}
	if payload == nil {
		return "", fmt.Errorf("%w: token produced no payload", ErrUnauthenticated)
	}
	if payload.Audience != v.Audience {
		return "", fmt.Errorf("%w: audience mismatch", ErrUnauthenticated)
	}
	if !googleIssuers[payload.Issuer] {
		return "", fmt.Errorf("%w: unexpected issuer", ErrUnauthenticated)
	}
	if verified, ok := payload.Claims["email_verified"].(bool); !ok || !verified {
		return "", fmt.Errorf("%w: email is not verified", ErrUnauthenticated)
	}
	if domain, ok := payload.Claims["hd"].(string); !ok || domain != RedHatDomain {
		return "", fmt.Errorf("%w: not a %s account", ErrUnauthenticated, RedHatDomain)
	}
	email, ok := payload.Claims["email"].(string)
	if !ok {
		return "", fmt.Errorf("%w: token carries no email", ErrUnauthenticated)
	}
	kerberos, err := KerberosFromEmail(email)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	return kerberos, nil
}

// KerberosFromEmail extracts the kerberos id from a redhat.com address. groups.yaml lists kerberos
// ids, so an address in any other shape cannot be matched against membership.
func KerberosFromEmail(email string) (string, error) {
	local, domain, found := strings.Cut(strings.ToLower(email), "@")
	if !found || domain != RedHatDomain {
		return "", fmt.Errorf("email is not a %s address", RedHatDomain)
	}
	if !kerberosPattern.MatchString(local) {
		return "", errors.New("email local part is not a kerberos id")
	}
	return local, nil
}

// BearerToken extracts the credential from an Authorization header value.
//
// The token is only ever read from this header. It is never accepted as a query parameter, which
// would put a live credential into access logs and proxy logs.
func BearerToken(header string) (string, error) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", errors.New("Authorization header is not a bearer token")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("bearer token is empty")
	}
	return token, nil
}

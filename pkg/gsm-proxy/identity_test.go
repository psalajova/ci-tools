package gsmproxy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/api/idtoken"
)

const testAudience = "https://gsm-proxy.ci.openshift.org"

// fakeValidator stands in for Google's signature check. It records the audience it was asked for,
// so a test can prove the verifier passes its own configured audience down rather than trusting
// whatever the token claims.
type fakeValidator struct {
	payload          *idtoken.Payload
	err              error
	requestedAudient string
}

func (f *fakeValidator) Validate(_ context.Context, _, audience string) (*idtoken.Payload, error) {
	f.requestedAudient = audience
	if f.err != nil {
		return nil, f.err
	}
	return f.payload, nil
}

// validPayload is a token that passes every check. Each table case mutates one thing about it.
func validPayload() *idtoken.Payload {
	return &idtoken.Payload{
		Issuer:   "https://accounts.google.com",
		Audience: testAudience,
		Claims: map[string]any{
			"email":          "jdoe@redhat.com",
			"email_verified": true,
			"hd":             RedHatDomain,
		},
	}
}

// TestGoogleIdentityVerifier is the token-rejection table.
//
// Every case is a token that must not authenticate anybody, plus the one case that must. The
// checks the idtoken library does not perform, on the issuer, the hosted domain and the verified
// flag, are the ones this table exists to pin.
func TestGoogleIdentityVerifier(t *testing.T) {
	for _, tc := range []struct {
		name             string
		mutate           func(*idtoken.Payload)
		validatorErr     error
		expectedKerberos string
	}{
		{
			name:             "a well formed Red Hat token authenticates",
			expectedKerberos: "jdoe",
		},
		{
			name:             "an address with dots keeps them, since groups.yaml uses them",
			mutate:           func(p *idtoken.Payload) { p.Claims["email"] = "jane.doe@redhat.com" },
			expectedKerberos: "jane.doe",
		},
		{
			name:             "the address is lowercased",
			mutate:           func(p *idtoken.Payload) { p.Claims["email"] = "JDoe@RedHat.com" },
			expectedKerberos: "jdoe",
		},
		{
			name:         "a signature or expiry failure is rejected",
			validatorErr: errors.New("idtoken: token expired"),
		},
		{
			name:   "a token minted for another audience is rejected",
			mutate: func(p *idtoken.Payload) { p.Audience = "https://some-other-service.ci.openshift.org" },
		},
		{
			name:   "an empty audience is rejected",
			mutate: func(p *idtoken.Payload) { p.Audience = "" },
		},
		{
			name:   "an audience differing only by a trailing slash is rejected",
			mutate: func(p *idtoken.Payload) { p.Audience = testAudience + "/" },
		},
		{
			name:   "a token from a non Google issuer is rejected",
			mutate: func(p *idtoken.Payload) { p.Issuer = "https://accounts.evil.example" },
		},
		{
			name:   "an issuer that merely contains the Google issuer is rejected",
			mutate: func(p *idtoken.Payload) { p.Issuer = "https://accounts.google.com.evil.example" },
		},
		{
			name:   "an empty issuer is rejected",
			mutate: func(p *idtoken.Payload) { p.Issuer = "" },
		},
		{
			name:   "an unverified email is rejected",
			mutate: func(p *idtoken.Payload) { p.Claims["email_verified"] = false },
		},
		{
			name:   "a missing email_verified claim is rejected",
			mutate: func(p *idtoken.Payload) { delete(p.Claims, "email_verified") },
		},
		{
			name:   "an email_verified claim that is a string is rejected",
			mutate: func(p *idtoken.Payload) { p.Claims["email_verified"] = "true" },
		},
		{
			name:   "a missing hosted domain is rejected, which is what a personal account has",
			mutate: func(p *idtoken.Payload) { delete(p.Claims, "hd") },
		},
		{
			name:   "a different hosted domain is rejected",
			mutate: func(p *idtoken.Payload) { p.Claims["hd"] = "example.com" },
		},
		{
			name:   "a hosted domain that merely ends in the real one is rejected",
			mutate: func(p *idtoken.Payload) { p.Claims["hd"] = "notredhat.com" },
		},
		{
			name: "a personal account claiming a Red Hat address is rejected",
			mutate: func(p *idtoken.Payload) {
				delete(p.Claims, "hd")
				p.Claims["email"] = "jdoe@redhat.com"
			},
		},
		{
			name:   "a missing email claim is rejected",
			mutate: func(p *idtoken.Payload) { delete(p.Claims, "email") },
		},
		{
			name:   "an email claim that is not a string is rejected",
			mutate: func(p *idtoken.Payload) { p.Claims["email"] = 42 },
		},
		{
			name:   "an address outside redhat.com is rejected even when hd is right",
			mutate: func(p *idtoken.Payload) { p.Claims["email"] = "jdoe@example.com" },
		},
		{
			name:   "an address whose domain only ends in redhat.com is rejected",
			mutate: func(p *idtoken.Payload) { p.Claims["email"] = "jdoe@notredhat.com" },
		},
		{
			name:   "a subdomain address is rejected",
			mutate: func(p *idtoken.Payload) { p.Claims["email"] = "jdoe@corp.redhat.com" },
		},
		{
			name:   "an address with two at signs is rejected",
			mutate: func(p *idtoken.Payload) { p.Claims["email"] = "jdoe@evil.example@redhat.com" },
		},
		{
			name:   "an empty local part is rejected",
			mutate: func(p *idtoken.Payload) { p.Claims["email"] = "@redhat.com" },
		},
		{
			name:   "a local part with a slash is rejected, since it could forge a path",
			mutate: func(p *idtoken.Payload) { p.Claims["email"] = "jdoe/../admin@redhat.com" },
		},
		{
			name:   "a local part with a plus tag is rejected",
			mutate: func(p *idtoken.Payload) { p.Claims["email"] = "jdoe+admin@redhat.com" },
		},
		{
			name:   "a local part starting with a dot is rejected",
			mutate: func(p *idtoken.Payload) { p.Claims["email"] = ".jdoe@redhat.com" },
		},
		{
			name:   "an address with no domain is rejected",
			mutate: func(p *idtoken.Payload) { p.Claims["email"] = "jdoe" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := validPayload()
			if tc.mutate != nil {
				tc.mutate(payload)
			}
			validator := &fakeValidator{payload: payload, err: tc.validatorErr}
			verifier := &GoogleIdentityVerifier{Audience: testAudience, Validator: validator}

			kerberos, err := verifier.Verify(context.Background(), "raw-token")

			if tc.expectedKerberos == "" {
				if err == nil {
					t.Fatalf("expected the token to be rejected, but it authenticated %q", kerberos)
				}
				if !errors.Is(err, ErrUnauthenticated) {
					t.Errorf("expected ErrUnauthenticated, got %v", err)
				}
				if kerberos != "" {
					t.Errorf("a rejected token must yield no kerberos id, got %q", kerberos)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected the token to be accepted, got %v", err)
			}
			if kerberos != tc.expectedKerberos {
				t.Errorf("expected kerberos %q, got %q", tc.expectedKerberos, kerberos)
			}
		})
	}
}

// TestGoogleIdentityVerifierPassesItsOwnAudience proves the audience handed to the signature check
// is the configured one, not anything derived from the request or the token.
func TestGoogleIdentityVerifierPassesItsOwnAudience(t *testing.T) {
	validator := &fakeValidator{payload: validPayload()}
	verifier := &GoogleIdentityVerifier{Audience: testAudience, Validator: validator}
	if _, err := verifier.Verify(context.Background(), "raw-token"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if validator.requestedAudient != testAudience {
		t.Errorf("expected the validator to be asked for %q, got %q", testAudience, validator.requestedAudient)
	}
}

// TestGoogleIdentityVerifierRequiresAnAudience makes a misconfigured proxy fail closed rather than
// accepting tokens minted for any service.
func TestGoogleIdentityVerifierRequiresAnAudience(t *testing.T) {
	verifier := &GoogleIdentityVerifier{Validator: &fakeValidator{payload: validPayload()}}
	if _, err := verifier.Verify(context.Background(), "raw-token"); err == nil {
		t.Fatal("expected a verifier with no audience to refuse every token")
	}
}

func TestGoogleIdentityVerifierRejectsANilPayload(t *testing.T) {
	verifier := &GoogleIdentityVerifier{Audience: testAudience, Validator: &fakeValidator{}}
	if _, err := verifier.Verify(context.Background(), "raw-token"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated for a nil payload, got %v", err)
	}
}

// TestVerifyErrorsOmitTheToken keeps a live credential out of the logs, which is where these
// errors end up.
func TestVerifyErrorsOmitTheToken(t *testing.T) {
	const rawToken = "eyJhbGciOiJSUzI1NiJ9.super-secret-token-body.signature"
	validator := &fakeValidator{err: errors.New("idtoken: invalid signature")}
	verifier := &GoogleIdentityVerifier{Audience: testAudience, Validator: validator}

	_, err := verifier.Verify(context.Background(), rawToken)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), rawToken) || strings.Contains(err.Error(), "super-secret-token-body") {
		t.Errorf("the error carries the token: %v", err)
	}
}

func TestBearerToken(t *testing.T) {
	for _, tc := range []struct {
		name, header, expected string
		expectError            bool
	}{
		{name: "a bearer token is extracted", header: "Bearer abc123", expected: "abc123"},
		{name: "the scheme is case insensitive", header: "bearer abc123", expected: "abc123"},
		{name: "surrounding space is trimmed", header: "Bearer   abc123  ", expected: "abc123"},
		{name: "an empty header is rejected", header: "", expectError: true},
		{name: "a bare token with no scheme is rejected", header: "abc123", expectError: true},
		{name: "basic auth is rejected", header: "Basic dXNlcjpwYXNz", expectError: true},
		{name: "a bearer scheme with no token is rejected", header: "Bearer ", expectError: true},
		{name: "a bearer scheme with only space is rejected", header: "Bearer    ", expectError: true},
		{name: "a scheme that merely starts with bearer is rejected", header: "Bearerish abc123", expectError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actual, err := BearerToken(tc.header)
			if tc.expectError {
				if err == nil {
					t.Fatalf("expected an error, got token %q", actual)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if actual != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, actual)
			}
		})
	}
}

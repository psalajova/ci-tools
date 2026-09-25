package gsmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sirupsen/logrus"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
)

// fakeVerifier accepts one token and rejects everything else.
type fakeVerifier struct {
	token    string
	kerberos string
}

func (f *fakeVerifier) Verify(_ context.Context, rawToken string) (string, error) {
	if rawToken != f.token {
		return "", ErrUnauthenticated
	}
	return f.kerberos, nil
}

type staticLister struct {
	ids []string
	err error
}

func (s *staticLister) ListSecretIDs(context.Context, string) ([]string, error) {
	return s.ids, s.err
}

func newTestHandler(t *testing.T, lister SecretLister, log *logrus.Entry) *ListSecretsHandler {
	t.Helper()
	if log == nil {
		log = logrus.NewEntry(logrus.New())
	}
	return &ListSecretsHandler{
		Verifier:   &fakeVerifier{token: "good-token", kerberos: "alice"},
		Authorizer: func() (*Authorizer, error) { return NewAuthorizer(testConfig(), testMembers()), nil },
		Lister:     lister,
		Log:        log,
	}
}

// serve drives the handler through a real mux so the {collection} wildcard is parsed the way it is
// in production rather than being set by hand.
func serve(handler *ListSecretsHandler, collection, authorization string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/collections/{collection}/secrets", handler)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/collections/"+collection+"/secrets", nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}

func TestListSecretsHandler(t *testing.T) {
	ids := []string{
		"alpha__aws__password",
		"alpha__aws__username",
		"alpha____index",
		"alpha__updater-service-account",
		"beta__aws__password",
	}

	for _, tc := range []struct {
		name           string
		collection     string
		authorization  string
		listerErr      error
		expectedStatus int
		expectedBody   any
	}{
		{
			name:           "a member gets the names in their collection",
			collection:     "alpha",
			authorization:  "Bearer good-token",
			expectedStatus: http.StatusOK,
			expectedBody:   ListSecretsResponse{Secrets: []string{"aws/password", "aws/username"}},
		},
		{
			name:           "a collection the caller does not own is forbidden",
			collection:     "beta",
			authorization:  "Bearer good-token",
			expectedStatus: http.StatusForbidden,
			expectedBody:   ErrorResponse{Error: MessageForbidden},
		},
		{
			name:           "a collection nobody owns is forbidden",
			collection:     "does-not-exist",
			authorization:  "Bearer good-token",
			expectedStatus: http.StatusForbidden,
			expectedBody:   ErrorResponse{Error: MessageForbidden},
		},
		{
			name:           "a missing Authorization header is unauthenticated",
			collection:     "alpha",
			expectedStatus: http.StatusUnauthorized,
			expectedBody:   ErrorResponse{Error: MessageUnauthenticated},
		},
		{
			name:           "a bad token is unauthenticated",
			collection:     "alpha",
			authorization:  "Bearer forged-token",
			expectedStatus: http.StatusUnauthorized,
			expectedBody:   ErrorResponse{Error: MessageUnauthenticated},
		},
		{
			name:           "basic auth is unauthenticated",
			collection:     "alpha",
			authorization:  "Basic dXNlcjpwYXNz",
			expectedStatus: http.StatusUnauthorized,
			expectedBody:   ErrorResponse{Error: MessageUnauthenticated},
		},
		{
			name:           "a malformed collection is a bad request",
			collection:     "Alpha",
			authorization:  "Bearer good-token",
			expectedStatus: http.StatusBadRequest,
			expectedBody:   ErrorResponse{Error: MessageBadCollection},
		},
		{
			name:           "a collection carrying the delimiter is a bad request",
			collection:     "alpha__aws",
			authorization:  "Bearer good-token",
			expectedStatus: http.StatusBadRequest,
			expectedBody:   ErrorResponse{Error: MessageBadCollection},
		},
		{
			name:           "an upstream failure is an opaque internal error",
			collection:     "alpha",
			authorization:  "Bearer good-token",
			listerErr:      errors.New("rpc error: permission denied on projects/openshift-ci-secrets/secrets/alpha__aws__password"),
			expectedStatus: http.StatusInternalServerError,
			expectedBody:   ErrorResponse{Error: MessageInternal},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := newTestHandler(t, &staticLister{ids: ids, err: tc.listerErr}, nil)
			recorder := serve(handler, tc.collection, tc.authorization)

			if recorder.Code != tc.expectedStatus {
				t.Errorf("expected status %d, got %d with body %s", tc.expectedStatus, recorder.Code, recorder.Body.String())
			}
			expected, err := json.Marshal(tc.expectedBody)
			if err != nil {
				t.Fatalf("failed to marshal the expected body: %v", err)
			}
			if diff := cmp.Diff(string(expected)+"\n", recorder.Body.String()); diff != "" {
				t.Errorf("unexpected body: %s", diff)
			}
		})
	}
}

// TestAuthenticationPrecedesAuthorization pins the order. An unauthenticated caller asking for a
// collection that does not exist must get a 401, never a 403, so an anonymous caller cannot probe
// which collections are real.
func TestAuthenticationPrecedesAuthorization(t *testing.T) {
	handler := newTestHandler(t, &staticLister{}, nil)
	for _, collection := range []string{"alpha", "beta", "does-not-exist", "Alpha"} {
		recorder := serve(handler, collection, "")
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("collection %q: expected 401 before any other check, got %d", collection, recorder.Code)
		}
	}
}

// TestForbiddenLooksTheSameForRealAndUnknownCollections stops the response from telling a caller
// which collections exist.
func TestForbiddenLooksTheSameForRealAndUnknownCollections(t *testing.T) {
	handler := newTestHandler(t, &staticLister{ids: []string{"beta__aws__password"}}, nil)

	real := serve(handler, "beta", "Bearer good-token")
	unknown := serve(handler, "does-not-exist", "Bearer good-token")

	if real.Code != unknown.Code {
		t.Errorf("a real collection answered %d and an unknown one %d", real.Code, unknown.Code)
	}
	if real.Body.String() != unknown.Body.String() {
		t.Errorf("the bodies differ:\nreal:    %s\nunknown: %s", real.Body.String(), unknown.Body.String())
	}
}

// TestUpstreamErrorsNeverReachTheClient keeps GCP resource names and project ids out of responses.
func TestUpstreamErrorsNeverReachTheClient(t *testing.T) {
	upstream := errors.New("rpc error: code = PermissionDenied on projects/openshift-ci-secrets/secrets/beta__aws__password")
	handler := newTestHandler(t, &staticLister{err: upstream}, nil)

	recorder := serve(handler, "alpha", "Bearer good-token")

	body := recorder.Body.String()
	for _, fragment := range []string{"openshift-ci-secrets", "beta__aws__password", "PermissionDenied", "rpc error"} {
		if strings.Contains(body, fragment) {
			t.Errorf("the response leaks %q: %s", fragment, body)
		}
	}
}

// TestNoSecretMetadataReachesTheResponseOrTheLog is the annotation-sentinel leak test end to end.
//
// Real GSM Secret values carrying the sentinel in their labels, annotations and etag are pushed
// through the production lister, the handler, the response writer and the logger. The sentinel
// must appear in none of it.
func TestNoSecretMetadataReachesTheResponseOrTheLog(t *testing.T) {
	iterator := &fakeIterator{secrets: []*secretmanagerpb.Secret{
		secretWithMetadata("alpha__aws__password"),
		secretWithMetadata("alpha__aws__username"),
		secretWithMetadata("beta__aws__password"),
	}}
	ids, err := secretIDsFrom(iterator)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var logged bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&logged)
	logger.SetLevel(logrus.DebugLevel)

	handler := newTestHandler(t, &staticLister{ids: ids}, logrus.NewEntry(logger))
	recorder := serve(handler, "alpha", "Bearer good-token")

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d with body %s", recorder.Code, recorder.Body.String())
	}
	var response ListSecretsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to decode the response: %v", err)
	}
	expected := []string{"aws/password", "aws/username"}
	if diff := cmp.Diff(expected, response.Secrets); diff != "" {
		t.Errorf("unexpected secrets: %s", diff)
	}
	if strings.Contains(recorder.Body.String(), sentinel) {
		t.Errorf("the response carries the sentinel: %s", recorder.Body.String())
	}
	if strings.Contains(logged.String(), sentinel) {
		t.Errorf("the log carries the sentinel: %s", logged.String())
	}
}

// TestSecretValuesAreNeverFetched guards the boundary between listing and reading. The proxy is
// allowed to know names; it must never hold a payload. A lister that only ever returns names is
// the whole interface the handler has, and this asserts it stays that way.
func TestSecretValuesAreNeverFetched(t *testing.T) {
	method, ok := reflect.TypeOf((*SecretLister)(nil)).Elem().MethodByName("ListSecretIDs")
	if !ok {
		t.Fatal("SecretLister has no ListSecretIDs method")
	}
	if n := method.Type.NumOut(); n != 2 {
		t.Fatalf("expected ListSecretIDs to return 2 values, got %d", n)
	}
	if out := method.Type.Out(0); out != reflect.TypeOf([]string(nil)) {
		t.Errorf("ListSecretIDs must return []string so no payload can cross the boundary, got %s", out)
	}
	if n := reflect.TypeOf((*SecretLister)(nil)).Elem().NumMethod(); n != 1 {
		t.Errorf("SecretLister must expose exactly one method, got %d", n)
	}
}

// TestListSecretsResponseCarriesOnlyNames is the structural test that the response type has no
// field that could carry metadata.
//
// GSM returns labels and annotations next to every name, and the CLI writes user-typed free text
// into two annotations. A future change that adds a map, a struct or an "any" to this type would
// create a channel for that text to reach the wire, so the shape is asserted rather than trusted.
func TestListSecretsResponseCarriesOnlyNames(t *testing.T) {
	responseType := reflect.TypeOf(ListSecretsResponse{})

	if n := responseType.NumField(); n != 1 {
		t.Fatalf("expected exactly 1 field, got %d", n)
	}
	field := responseType.Field(0)
	if field.Name != "Secrets" {
		t.Errorf("expected the only field to be Secrets, got %s", field.Name)
	}
	if field.Type != reflect.TypeOf([]string(nil)) {
		t.Errorf("expected Secrets to be []string, got %s", field.Type)
	}
	if tag := field.Tag.Get("json"); tag != "secrets" {
		t.Errorf("expected the json tag to be \"secrets\", got %q", tag)
	}

	assertOnlyStringsReachable(t, responseType)
}

// assertOnlyStringsReachable walks a type and fails if anything but strings, slices of them and
// structs of them can be serialized. Maps, interfaces and []byte are the shapes that could carry
// secret metadata or a payload, so none of them may be reachable.
func assertOnlyStringsReachable(t *testing.T, typ reflect.Type) {
	t.Helper()
	switch typ.Kind() {
	case reflect.String:
		return
	case reflect.Slice, reflect.Array, reflect.Ptr:
		if typ.Kind() == reflect.Slice && typ.Elem().Kind() == reflect.Uint8 {
			t.Errorf("%s is a byte slice, which could carry a secret payload", typ)
			return
		}
		assertOnlyStringsReachable(t, typ.Elem())
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			assertOnlyStringsReachable(t, typ.Field(i).Type)
		}
	default:
		t.Errorf("%s has kind %s; only strings and containers of strings may be reachable from a response", typ, typ.Kind())
	}
}

// TestErrorResponseCarriesOnlyAFixedMessage applies the same rule to the error shape, and pins
// that every message the proxy can emit is a constant rather than a formatted upstream error.
func TestErrorResponseCarriesOnlyAFixedMessage(t *testing.T) {
	errorType := reflect.TypeOf(ErrorResponse{})
	if n := errorType.NumField(); n != 1 {
		t.Fatalf("expected exactly 1 field, got %d", n)
	}
	assertOnlyStringsReachable(t, errorType)

	for _, message := range []string{MessageUnauthenticated, MessageForbidden, MessageBadCollection, MessageInternal} {
		if strings.ContainsAny(message, "%") {
			t.Errorf("message %q contains a format verb, so it could interpolate upstream detail", message)
		}
	}
}

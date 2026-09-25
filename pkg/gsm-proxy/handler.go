package gsmproxy

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/sirupsen/logrus"

	gsmvalidation "github.com/openshift/ci-tools/pkg/gsm-validation"
)

type ListSecretsResponse struct {
	Secrets []string `json:"secrets"`
}

// ErrorResponse carries a fixed message. Upstream GCP errors are logged, never returned: they can
// name resources and project numbers.
type ErrorResponse struct {
	Error string `json:"error"`
}

const (
	MessageUnauthenticated = "a valid Google identity token is required; run 'gcloud auth login' and try again"
	MessageForbidden       = "you are not a member of a rover group that owns this secret collection"
	MessageBadCollection   = "the secret collection name is malformed"
	MessageInternal        = "the request could not be completed"
)

// TokenVerifier turns a bearer token into the kerberos id of the person who presented it, or
// returns an error wrapping ErrUnauthenticated.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (string, error)
}

// ListSecretsHandler serves the secret names of one collection to a member of a rover group that owns it.
type ListSecretsHandler struct {
	Verifier TokenVerifier
	Lister   SecretLister
	// Authorizer is called per request rather than held, because the two files behind it are
	// mounted ConfigMaps that change underneath the process; see LoadAuthorizer.
	Authorizer func() (*Authorizer, error)
	Log        *logrus.Entry
}

func (handler *ListSecretsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	collection := r.PathValue("collection")

	token, err := BearerToken(r.Header.Get("Authorization"))
	if err != nil {
		handler.Log.WithError(err).Debug("rejected a request without a usable bearer token")
		writeError(w, http.StatusUnauthorized, MessageUnauthenticated)
		return
	}
	kerberosID, err := handler.Verifier.Verify(r.Context(), token)
	if err != nil {
		handler.Log.WithError(err).Info("rejected a request with an invalid identity token")
		writeError(w, http.StatusUnauthorized, MessageUnauthenticated)
		return
	}

	log := handler.Log.WithFields(logrus.Fields{"kerberosID": kerberosID, "collection": collection})

	if !gsmvalidation.ValidateCollectionName(collection) {
		writeError(w, http.StatusBadRequest, MessageBadCollection)
		return
	}
	authorizer, err := handler.Authorizer()
	if err != nil {
		log.WithError(err).Error("failed to load authorization data")
		writeError(w, http.StatusInternalServerError, MessageInternal)
		return
	}
	if err := authorizer.AuthorizeCollectionAccess(kerberosID, collection); err != nil {
		log.WithError(err).Info("denied a request")
		writeError(w, http.StatusForbidden, MessageForbidden)
		return
	}

	ids, err := handler.Lister.ListSecretIDs(r.Context(), collection)
	if err != nil {
		log.WithError(err).Error("failed to list secrets")
		writeError(w, http.StatusInternalServerError, MessageInternal)
		return
	}
	paths := SecretPathsForCollection(ids, collection)
	writeJSON(w, http.StatusOK, ListSecretsResponse{Secrets: paths})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		logrus.WithError(err).Error("failed to write response")
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, ErrorResponse{Error: message})
}

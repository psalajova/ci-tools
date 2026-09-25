package gsmproxy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/iterator"

	gsmsecrets "github.com/openshift/ci-tools/pkg/gsm-secrets"
	gsmvalidation "github.com/openshift/ci-tools/pkg/gsm-validation"
)

// secretIterator is the part of *secretmanager.SecretIterator that GSMLister uses.
type secretIterator interface {
	Next() (*secretmanagerpb.Secret, error)
}

// SecretLister yields the bare secret IDs of the secrets that may belong to one collection.
//
// "May belong to" is deliberate: the result is a superset, and SecretPathsForCollection is what
// decides membership. See GSMLister.ListSecretIDs.
//
// The return type is []string rather than the GCP Secret type on purpose. A Secret carries labels
// and annotations, and the CLI stores free text typed by users in "rotation-instructions" and
// "request-information"; projecting to the ID behind this interface means no caller can leak that
// metadata into a response or a log line, because no caller ever sees it.
type SecretLister interface {
	ListSecretIDs(ctx context.Context, collection string) ([]string, error)
}

// GSMLister lists secret IDs straight from GSM on every call, with no cache.
//
// The project holds ~10k secrets and an unfiltered listing takes about 10s, which is too slow to
// serve uncached. Rather than cache the whole project, the request is narrowed with the API's
// server-side filter, which brings the same listing down to under 2s.
//
// The filter is a performance measure and nothing else. GSM's "name:" operator matches the value
// as a substring anywhere in the name, not as a prefix: "name:azure__" also returns
// "dev-azure____index" and "fxie__hypershift-ext-dns-app-azure__credentials". So the result is a
// superset that can contain other collections' names, and SecretPathsForCollection stays the sole
// authority on membership via an exact comparison of the extracted collection.
//
// What the filter must never do is under-match, because that would silently hide a caller's own
// secrets. It cannot: every secret in collection C begins with "C__", so "C__" is a substring of
// all of them. Verified against production on 2026-09-25, where the filtered result for
// test-platform-infra was byte-identical to the 970 names an unfiltered listing yields.
type GSMLister struct {
	Client  gsmsecrets.SecretManagerClient
	Project string
}

func (l *GSMLister) ListSecretIDs(ctx context.Context, collection string) ([]string, error) {
	// Revalidated here rather than trusted from the handler, so the filter expression cannot be
	// shaped by the caller even if a future caller forgets to check. The charset is [a-z0-9_-],
	// which carries no filter metacharacters.
	if !gsmvalidation.ValidateCollectionName(collection) {
		return nil, fmt.Errorf("refusing to list secrets for malformed collection name %q", collection)
	}
	return secretIDsFrom(l.Client.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{
		Parent: fmt.Sprintf("projects/%s", l.Project),
		Filter: collectionFilter(collection),
	}))
}

// collectionFilter is the GSM list filter that narrows a listing to one collection's secrets, plus
// whatever else happens to contain the same substring.
func collectionFilter(collection string) string {
	return "name:" + collection + gsmvalidation.CollectionSecretDelimiter
}

// secretIDsFrom drains an iterator of GSM secrets down to their IDs.
//
// This is the one place in the proxy where a *secretmanagerpb.Secret is in scope, and it is split
// out from the client call so a test can push real Secret values carrying labels and annotations
// through the production projection and prove nothing but the name survives.
func secretIDsFrom(it secretIterator) ([]string, error) {
	var ids []string
	for {
		secret, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return ids, nil
		}
		if err != nil {
			return nil, fmt.Errorf("failed to list secrets: %w", err)
		}
		ids = append(ids, secretID(secret.GetName()))
	}
}

// secretID reduces a fully qualified resource name, "projects/P/secrets/S", to "S".
func secretID(resourceName string) string {
	if i := strings.LastIndex(resourceName, "/"); i >= 0 {
		return resourceName[i+1:]
	}
	return resourceName
}

// SecretPathsForCollection returns the "group/field" paths of the secrets in one collection.
func SecretPathsForCollection(ids []string, collection string) []string {
	if !gsmvalidation.ValidateCollectionName(collection) {
		return []string{}
	}
	prefix := collection + gsmvalidation.CollectionSecretDelimiter
	paths := make([]string, 0, len(ids))
	for _, id := range ids {
		if gsmsecrets.ExtractCollectionFromSecretName(id) != collection {
			continue
		}
		if gsmsecrets.ClassifySecret(id) != gsmsecrets.SecretTypeGeneric {
			continue
		}
		rest := strings.TrimPrefix(id, prefix)
		if rest == "" || rest == id {
			continue
		}
		paths = append(paths, strings.ReplaceAll(rest, gsmvalidation.CollectionSecretDelimiter, "/"))
	}
	sort.Strings(paths)
	return paths
}

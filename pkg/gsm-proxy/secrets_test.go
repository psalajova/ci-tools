package gsmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/iterator"
)

// sentinel is a string that must never leave the proxy. Tests plant it in every part of a GSM
// Secret that is not the name and then assert it appears nowhere in any output.
const sentinel = "LEAKED-SENTINEL-do-not-return-this"

// fakeIterator replays a fixed list of GSM secrets, then an error.
type fakeIterator struct {
	secrets []*secretmanagerpb.Secret
	err     error
	i       int
}

func (f *fakeIterator) Next() (*secretmanagerpb.Secret, error) {
	if f.i >= len(f.secrets) {
		if f.err != nil {
			return nil, f.err
		}
		return nil, iterator.Done
	}
	s := f.secrets[f.i]
	f.i++
	return s, nil
}

// secretWithMetadata builds a GSM Secret shaped like the ones the CLI creates: a name plus labels
// and annotations holding free text a user typed.
func secretWithMetadata(id string) *secretmanagerpb.Secret {
	return &secretmanagerpb.Secret{
		Name: "projects/openshift-ci-secrets/secrets/" + id,
		Labels: map[string]string{
			"jira-project": sentinel,
			"owner":        sentinel,
		},
		Annotations: map[string]string{
			"rotation-instructions": sentinel,
			"request-information":   sentinel,
		},
		Etag: sentinel,
	}
}

// TestSecretIDsFromDropsAllMetadata is the annotation-sentinel leak test at its source.
//
// secretIDsFrom is the only function in the proxy that holds a *secretmanagerpb.Secret. Real
// Secret values carrying the sentinel in their labels, annotations and etag go through the
// production projection, and nothing but the bare ID may come out.
func TestSecretIDsFromDropsAllMetadata(t *testing.T) {
	it := &fakeIterator{secrets: []*secretmanagerpb.Secret{
		secretWithMetadata("my-collection__aws__password"),
		secretWithMetadata("my-collection__gcp__token"),
		secretWithMetadata("other-collection__aws__password"),
	}}

	ids, err := secretIDsFrom(it)
	if err != nil {
		t.Fatalf("secretIDsFrom returned an error: %v", err)
	}

	expected := []string{
		"my-collection__aws__password",
		"my-collection__gcp__token",
		"other-collection__aws__password",
	}
	if diff := cmp.Diff(expected, ids); diff != "" {
		t.Errorf("unexpected ids: %s", diff)
	}
	for _, id := range ids {
		if strings.Contains(id, sentinel) {
			t.Errorf("id %q carries the sentinel", id)
		}
	}
	// Serializing the result is the shape it reaches the wire in; the sentinel must not survive it.
	encoded, err := json.Marshal(ids)
	if err != nil {
		t.Fatalf("failed to marshal ids: %v", err)
	}
	if strings.Contains(string(encoded), sentinel) {
		t.Errorf("the serialized ids carry the sentinel: %s", encoded)
	}
}

func TestSecretIDsFromPropagatesErrors(t *testing.T) {
	upstream := errors.New("rpc error: permission denied on projects/openshift-ci-secrets")
	it := &fakeIterator{secrets: []*secretmanagerpb.Secret{secretWithMetadata("c__a__b")}, err: upstream}

	if _, err := secretIDsFrom(it); !errors.Is(err, upstream) {
		t.Errorf("expected the upstream error to be wrapped, got %v", err)
	}
}

// TestCollectionFilterNeverUnderMatches pins the property the server-side filter rests on.
//
// GSM's "name:" operator matches a substring, so the filter returns a superset and
// SecretPathsForCollection remains the authority on membership. What would be a real defect is
// under-matching, because a caller would silently lose their own secrets. Every secret in a
// collection begins with "<collection>__", so the filter value is a substring of all of them.
func TestCollectionFilterNeverUnderMatches(t *testing.T) {
	for _, collection := range []string{"alpha", "my-collection", "my-collection-2", "azure", "a"} {
		t.Run(collection, func(t *testing.T) {
			filter := collectionFilter(collection)
			value, ok := strings.CutPrefix(filter, "name:")
			if !ok {
				t.Fatalf("expected the filter to use the name operator, got %q", filter)
			}
			for _, id := range []string{
				collection + "__a__b",
				collection + "____index",
				collection + "__updater-service-account",
			} {
				if !strings.Contains(id, value) {
					t.Errorf("filter value %q does not match %q, which belongs to the collection", value, id)
				}
			}
		})
	}
}

func TestListSecretIDsRejectsMalformedCollections(t *testing.T) {
	// A nil client is deliberate: a malformed name must be refused before any GCP call is made.
	lister := &GSMLister{Project: "openshift-ci-secrets"}

	for _, collection := range []string{"", "__", "my-collection__aws", "MY-COLLECTION", "_leading", "trailing_"} {
		t.Run(collection, func(t *testing.T) {
			if _, err := lister.ListSecretIDs(context.Background(), collection); err == nil {
				t.Errorf("expected collection %q to be rejected", collection)
			}
		})
	}
}

func TestSecretID(t *testing.T) {
	for _, tc := range []struct{ name, in, expected string }{
		{name: "fully qualified", in: "projects/openshift-ci-secrets/secrets/c__a__b", expected: "c__a__b"},
		{name: "bare id", in: "c__a__b", expected: "c__a__b"},
		{name: "trailing slash", in: "projects/p/secrets/", expected: ""},
		{name: "empty", in: "", expected: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if actual := secretID(tc.in); actual != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, actual)
			}
		})
	}
}

// TestSecretPathsForCollection is the adversarial collection-name table.
//
// Every case is a way one collection's names could bleed into another collection's listing:
// prefix and substring overlap, the reserved index and updater-service-account secrets, names
// with no delimiter, and names whose collection part is not a legal collection name.
func TestSecretPathsForCollection(t *testing.T) {
	// ids is the whole project as the proxy sees it, shared by every case so that each case proves
	// what one caller gets out of the same unfiltered input.
	ids := []string{
		"my-collection__aws__password",
		"my-collection__aws__username",
		"my-collection__token",
		"my-collection____index",
		"my-collection__updater-service-account",
		"my-collection-2__aws__password",
		"my-collectionx__aws__password",
		"my__aws__password",
		"other__my-collection__password",
		"no-delimiter-secret",
		"my-collection__",
		"__my-collection__leading",
		"MY-COLLECTION__aws__password",
	}

	for _, tc := range []struct {
		name       string
		collection string
		expected   []string
	}{
		{
			name:       "exact collection only, reserved secrets excluded",
			collection: "my-collection",
			expected:   []string{"aws/password", "aws/username", "token"},
		},
		{
			name:       "a dash suffix is a different collection",
			collection: "my-collection-2",
			expected:   []string{"aws/password"},
		},
		{
			name:       "a letter suffix is a different collection",
			collection: "my-collectionx",
			expected:   []string{"aws/password"},
		},
		{
			name:       "a prefix of another collection is its own collection",
			collection: "my",
			expected:   []string{"aws/password"},
		},
		{
			name:       "the collection name appearing as a group does not match",
			collection: "other",
			expected:   []string{"my-collection/password"},
		},
		{
			name:       "an unknown collection gets nothing",
			collection: "does-not-exist",
			expected:   []string{},
		},
		{
			name:       "the empty collection gets nothing",
			collection: "",
			expected:   []string{},
		},
		{
			name:       "the delimiter as a collection gets nothing",
			collection: "__",
			expected:   []string{},
		},
		{
			name:       "a collection spelled with the delimiter gets nothing",
			collection: "my-collection__aws",
			expected:   []string{},
		},
		{
			name:       "matching is case sensitive",
			collection: "MY-COLLECTION",
			expected:   []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actual := SecretPathsForCollection(ids, tc.collection)
			if diff := cmp.Diff(tc.expected, actual); diff != "" {
				t.Errorf("unexpected paths: %s", diff)
			}
		})
	}
}

// TestSecretPathsForCollectionPartitionsTheProject asserts the property behind the table: every
// served path reconstructs to a secret that exists and that no other collection also served.
//
// A path is relative to its collection, so "a/b" legitimately appears under many collections. The
// invariant is about the underlying secret, so each path is mapped back to its GSM id the way the
// CLI builds one, with "/" turned into the delimiter and the collection prefixed.
func TestSecretPathsForCollectionPartitionsTheProject(t *testing.T) {
	ids := []string{
		"alpha__a__b", "alpha-two__a__b", "alphatwo__a__b", "alph__a__b",
		"beta__a__b", "beta____index", "beta__updater-service-account",
		"no-delimiter", "__leading__delimiter",
	}
	known := map[string]bool{}
	for _, id := range ids {
		known[id] = true
	}
	collections := []string{"alpha", "alpha-two", "alphatwo", "alph", "beta", "a", "b", "", "__", "gamma"}

	servedBy := map[string]string{}
	for _, collection := range collections {
		for _, path := range SecretPathsForCollection(ids, collection) {
			id := collection + "__" + strings.ReplaceAll(path, "/", "__")
			if !known[id] {
				t.Errorf("collection %q served path %q, which reconstructs to %q, a secret that does not exist", collection, path, id)
				continue
			}
			if previous, ok := servedBy[id]; ok {
				t.Errorf("secret %q is served to both %q and %q", id, previous, collection)
			}
			servedBy[id] = collection
		}
	}

	// The reserved secrets and the two malformed names must be served to nobody.
	for _, id := range []string{"beta____index", "beta__updater-service-account", "no-delimiter", "__leading__delimiter"} {
		if collection, ok := servedBy[id]; ok {
			t.Errorf("secret %q must never be served, but %q got it", id, collection)
		}
	}
}

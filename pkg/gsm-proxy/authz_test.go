package gsmproxy

import (
	"errors"
	"os"
	"testing"

	"github.com/openshift/ci-tools/pkg/group"
)

func testConfig() *group.Config {
	return &group.Config{Groups: map[string]group.Target{
		"team-alpha": {SecretCollections: []string{"alpha", "shared"}},
		"team-beta":  {SecretCollections: []string{"beta", "shared"}},
		"team-empty": {},
		"orphans":    {Unclaimed: true, SecretCollections: []string{"abandoned", "alpha"}},
	}}
}

func testMembers() map[string][]string {
	return map[string][]string{
		"team-alpha": {"alice", "carol"},
		"team-beta":  {"bob"},
		"team-empty": {"dave"},
		"orphans":    {"mallory"},
	}
}

func TestAuthorize(t *testing.T) {
	a := NewAuthorizer(testConfig(), testMembers())

	for _, tc := range []struct {
		name, kerberos, collection string
		allowed                    bool
	}{
		{name: "a member reaches their own collection", kerberos: "alice", collection: "alpha", allowed: true},
		{name: "a second member of the same group reaches it", kerberos: "carol", collection: "alpha", allowed: true},
		{name: "a member of either owning group reaches a shared collection", kerberos: "alice", collection: "shared", allowed: true},
		{name: "and so does the other one", kerberos: "bob", collection: "shared", allowed: true},
		{name: "a member of another group is denied", kerberos: "bob", collection: "alpha"},
		{name: "a member of a group owning nothing is denied", kerberos: "dave", collection: "alpha"},
		{name: "an unknown kerberos id is denied", kerberos: "eve", collection: "alpha"},
		{name: "an unknown collection is denied", kerberos: "alice", collection: "does-not-exist"},
		{name: "an empty kerberos id is denied", kerberos: "", collection: "alpha"},
		{name: "an empty collection is denied", kerberos: "alice", collection: ""},
		{
			name:       "a collection owned only by an unclaimed group is denied",
			kerberos:   "mallory",
			collection: "abandoned",
		},
		{
			name:       "an unclaimed group grants nothing even for a collection a real group owns",
			kerberos:   "mallory",
			collection: "alpha",
		},
		{name: "a rover group name is not a collection name", kerberos: "alice", collection: "team-alpha"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := a.AuthorizeCollectionAccess(tc.kerberos, tc.collection)
			if tc.allowed {
				if err != nil {
					t.Fatalf("expected access to be allowed, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected access to be denied")
			}
			if !errors.Is(err, ErrForbidden) {
				t.Errorf("expected ErrForbidden, got %v", err)
			}
		})
	}
}

// TestAuthorizeIgnoresUnclaimedGroupsEntirely pins the property the table samples: a collection
// listed only under an unclaimed group is unknown to the authorizer, because gsm-secret-sync
// writes no IAM bindings for one either.
func TestAuthorizeIgnoresUnclaimedGroupsEntirely(t *testing.T) {
	a := NewAuthorizer(testConfig(), testMembers())
	if _, ok := a.collectionOwners["abandoned"]; ok {
		t.Error("an unclaimed collection must not be indexed at all")
	}
	if owners := a.collectionOwners["alpha"]; owners.Has("orphans") {
		t.Errorf("an unclaimed group must not be recorded as an owner, got %v", owners)
	}
}

// TestAuthorizeDoesNotTrustMembershipAlone proves the lookup order. A group present in groups.yaml
// but absent from _config.yaml grants nothing, so a stray LDAP group cannot introduce access.
func TestAuthorizeDoesNotTrustMembershipAlone(t *testing.T) {
	members := testMembers()
	members["group-not-in-config"] = []string{"eve"}
	a := NewAuthorizer(testConfig(), members)

	for _, collection := range []string{"alpha", "beta", "shared", "abandoned", "group-not-in-config"} {
		if err := a.AuthorizeCollectionAccess("eve", collection); err == nil {
			t.Errorf("a group absent from the config granted access to %q", collection)
		}
	}
}

func TestLoadAuthorizerRejectsEmptyInputs(t *testing.T) {
	dir := t.TempDir()
	config := dir + "/_config.yaml"
	groups := dir + "/groups.yaml"
	writeFile := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", path, err)
		}
	}

	t.Run("an empty groups file is refused rather than denying everyone", func(t *testing.T) {
		writeFile(t, config, "groups:\n  team-alpha:\n    secret_collections:\n    - alpha\n")
		writeFile(t, groups, "{}\n")
		if _, err := LoadAuthorizer(config, groups); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("a config with no groups is refused", func(t *testing.T) {
		writeFile(t, config, "{}\n")
		writeFile(t, groups, "team-alpha:\n- alice\n")
		if _, err := LoadAuthorizer(config, groups); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("a well formed pair loads", func(t *testing.T) {
		writeFile(t, config, "groups:\n  team-alpha:\n    secret_collections:\n    - alpha\n")
		writeFile(t, groups, "team-alpha:\n- alice\n")
		a, err := LoadAuthorizer(config, groups)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if err := a.AuthorizeCollectionAccess("alice", "alpha"); err != nil {
			t.Errorf("expected alice to reach alpha, got %v", err)
		}
		if err := a.AuthorizeCollectionAccess("bob", "alpha"); err == nil {
			t.Error("expected bob to be denied")
		}
	})
}

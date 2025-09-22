package gsmassmigration

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	gsmsecrets "github.com/openshift/ci-tools/pkg/gsm-secrets"
)

func TestBuildIndexUpdates(t *testing.T) {
	testCases := []struct {
		name                 string
		migrations           []MigrationResult
		existingByCollection map[string][]string
		wantCollections      []string
		wantEntries          map[string][]string // collection -> expected entries (parsed back, without updater-service-account)
	}{
		{
			name: "adds new entries to empty index",
			migrations: []MigrationResult{
				{
					Collection:    "my-collection",
					CreatedFields: []string{"my-collection__group__field1", "my-collection__group__field2"},
				},
			},
			existingByCollection: map[string][]string{},
			wantCollections:      []string{"my-collection"},
			wantEntries: map[string][]string{
				"my-collection": {"group__field1", "group__field2"},
			},
		},
		{
			name: "merges with existing index entries",
			migrations: []MigrationResult{
				{
					Collection:    "my-collection",
					CreatedFields: []string{"my-collection__group__field2"},
				},
			},
			existingByCollection: map[string][]string{
				"my-collection": {"group__field1"},
			},
			wantCollections: []string{"my-collection"},
			wantEntries: map[string][]string{
				"my-collection": {"group__field1", "group__field2"},
			},
		},
		{
			name: "deduplicates entries already in index",
			migrations: []MigrationResult{
				{
					Collection:    "my-collection",
					CreatedFields: []string{"my-collection__group__field1"},
				},
			},
			existingByCollection: map[string][]string{
				"my-collection": {"group__field1"},
			},
			wantCollections: nil,
		},
		{
			name: "skips failed migrations",
			migrations: []MigrationResult{
				{
					Collection:    "my-collection",
					CreatedFields: []string{"my-collection__group__field1"},
					Error:         fmt.Errorf("failed"),
				},
			},
			existingByCollection: map[string][]string{},
			wantCollections:      nil,
		},
		{
			name: "handles multiple collections",
			migrations: []MigrationResult{
				{
					Collection:    "coll-a",
					CreatedFields: []string{"coll-a__g1__f1"},
				},
				{
					Collection:    "coll-b",
					CreatedFields: []string{"coll-b__g2__f2", "coll-b__g2__f3"},
				},
			},
			existingByCollection: map[string][]string{},
			wantCollections:      []string{"coll-a", "coll-b"},
			wantEntries: map[string][]string{
				"coll-a": {"g1__f1"},
				"coll-b": {"g2__f2", "g2__f3"},
			},
		},
		{
			name: "strips nested group paths correctly",
			migrations: []MigrationResult{
				{
					Collection:    "psalajova-first-secret",
					CreatedFields: []string{"psalajova-first-secret__ibmcloud-rhoai-qe-collection__cluster-secrets__ssh-privatekey"},
				},
			},
			existingByCollection: map[string][]string{
				"psalajova-first-secret": {"group1__test-secret-06003"},
			},
			wantCollections: []string{"psalajova-first-secret"},
			wantEntries: map[string][]string{
				"psalajova-first-secret": {"group1__test-secret-06003", "ibmcloud-rhoai-qe-collection__cluster-secrets__ssh-privatekey"},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := buildIndexUpdates(tc.migrations, tc.existingByCollection)

			if tc.wantCollections == nil {
				if len(result) != 0 {
					t.Errorf("expected no updates, got %d collections", len(result))
				}
				return
			}

			if len(result) != len(tc.wantCollections) {
				t.Fatalf("expected %d collections, got %d", len(tc.wantCollections), len(result))
			}

			for _, collection := range tc.wantCollections {
				payload, ok := result[collection]
				if !ok {
					t.Errorf("missing collection %s in result", collection)
					continue
				}
				gotEntries := gsmsecrets.ParseIndexSecretContent(payload)
				wantEntries := tc.wantEntries[collection]
				if diff := cmp.Diff(wantEntries, gotEntries); diff != "" {
					t.Errorf("collection %s entries mismatch (-want +got):\n%s", collection, diff)
				}
			}
		})
	}
}

package multi_stage

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/secrets-store-csi-driver-provider-gcp/config"
	"github.com/google/go-cmp/cmp"

	"sigs.k8s.io/yaml"

	"github.com/openshift/ci-tools/pkg/api"
)

func TestGroupCredentialsByCollectionAndMountPath(t *testing.T) {
	testCases := []struct {
		name        string
		credentials []api.CredentialReference
		expected    map[string][]api.CredentialReference
	}{
		{
			name:        "empty credentials",
			credentials: []api.CredentialReference{},
			expected:    map[string][]api.CredentialReference{},
		},
		{
			name: "single credential",
			credentials: []api.CredentialReference{
				{Name: "cred1", Collection: "collection1", MountPath: "/tmp/cred1"},
			},
			expected: map[string][]api.CredentialReference{
				"collection1:/tmp/cred1": {
					{Name: "cred1", Collection: "collection1", MountPath: "/tmp/cred1"},
				},
			},
		},
		{
			name: "multiple credentials different collections and paths",
			credentials: []api.CredentialReference{
				{Name: "cred1", Collection: "collection1", MountPath: "/tmp/cred1"},
				{Name: "cred2", Collection: "collection2", MountPath: "/tmp/cred2"},
			},
			expected: map[string][]api.CredentialReference{
				"collection1:/tmp/cred1": {
					{Name: "cred1", Collection: "collection1", MountPath: "/tmp/cred1"},
				},
				"collection2:/tmp/cred2": {
					{Name: "cred2", Collection: "collection2", MountPath: "/tmp/cred2"},
				},
			},
		},
		{
			name: "multiple credentials same collection and path",
			credentials: []api.CredentialReference{
				{Name: "cred1", Collection: "collection1", MountPath: "/tmp/shared"},
				{Name: "cred2", Collection: "collection1", MountPath: "/tmp/shared"},
			},
			expected: map[string][]api.CredentialReference{
				"collection1:/tmp/shared": {
					{Name: "cred1", Collection: "collection1", MountPath: "/tmp/shared"},
					{Name: "cred2", Collection: "collection1", MountPath: "/tmp/shared"},
				},
			},
		},
		{
			name: "mixed grouping - some grouped together, some separate",
			credentials: []api.CredentialReference{
				{Name: "red", Collection: "colours", MountPath: "/tmp/path"},
				{Name: "blue", Collection: "colours", MountPath: "/tmp/path"},
				{Name: "circle", Collection: "shapes", MountPath: "/tmp/path"},
				{Name: "square", Collection: "shapes", MountPath: "/tmp/other"},
			},
			expected: map[string][]api.CredentialReference{
				"colours:/tmp/path": {
					{Name: "red", Collection: "colours", MountPath: "/tmp/path"},
					{Name: "blue", Collection: "colours", MountPath: "/tmp/path"},
				},
				"shapes:/tmp/path": {
					{Name: "circle", Collection: "shapes", MountPath: "/tmp/path"},
				},
				"shapes:/tmp/other": {
					{Name: "square", Collection: "shapes", MountPath: "/tmp/other"},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := groupCredentialsByCollectionAndMountPath(tc.credentials)
			if diff := cmp.Diff(tc.expected, result); diff != "" {
				t.Errorf("groupCredentialsByCollectionAndMountPath() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBuildGCPSecretsParameter(t *testing.T) {
	testCases := []struct {
		name        string
		credentials []api.CredentialReference
		expected    []config.Secret
	}{
		{
			name:        "empty credentials",
			credentials: []api.CredentialReference{},
			expected:    nil,
		},
		{
			name: "single credential",
			credentials: []api.CredentialReference{
				{Name: "cred1", Collection: "collection1"},
			},
			expected: []config.Secret{
				{
					ResourceName: fmt.Sprintf("projects/%s/secrets/collection1__cred1/versions/latest", GSMproject),
					FileName:     "cred1",
				},
			},
		},
		{
			name: "multiple credentials",
			credentials: []api.CredentialReference{
				{Name: "cred1", Collection: "collection1"},
				{Name: "cred2", Collection: "collection2"},
			},
			expected: []config.Secret{
				{
					ResourceName: fmt.Sprintf("projects/%s/secrets/collection1__cred1/versions/latest", GSMproject),
					FileName:     "cred1",
				},
				{
					ResourceName: fmt.Sprintf("projects/%s/secrets/collection2__cred2/versions/latest", GSMproject),
					FileName:     "cred2",
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			yamlString, err := buildGCPSecretsParameter(tc.credentials)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var actual []config.Secret
			err = yaml.Unmarshal([]byte(yamlString), &actual)
			if err != nil {
				t.Fatalf("Failed to unmarshal YAML output: %v", err)
			}

			if diff := cmp.Diff(tc.expected, actual); diff != "" {
				t.Errorf("buildGCPSecretsParameter() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGetSPCName(t *testing.T) {
	testCases := []struct {
		name        string
		namespace   string
		collection  string
		mountPath   string
		credentials []api.CredentialReference
	}{
		{
			name:       "simple case",
			namespace:  "test-ns",
			collection: "collection1",
			mountPath:  "/tmp/cred1",
			credentials: []api.CredentialReference{
				{Name: "cred1", Collection: "collection1", MountPath: "/tmp/cred1"},
			},
		},
		{
			name:       "typical ci-operator namespace",
			namespace:  "ci-op-abc123def456",
			collection: "collection1",
			mountPath:  "/tmp/cred1",
			credentials: []api.CredentialReference{
				{Name: "cred1", Collection: "collection1", MountPath: "/tmp/cred1"},
			},
		},
		{
			name:       "multiple credentials same collection and path",
			namespace:  "test-ns",
			collection: "collection1",
			mountPath:  "/tmp/shared",
			credentials: []api.CredentialReference{
				{Name: "cred1", Collection: "collection1", MountPath: "/tmp/shared"},
				{Name: "cred2", Collection: "collection1", MountPath: "/tmp/shared"},
			},
		},
		{
			name:       "different credentials should give different names",
			namespace:  "test-ns",
			collection: "collection1",
			mountPath:  "/tmp/shared",
			credentials: []api.CredentialReference{
				{Name: "cred1", Collection: "collection1", MountPath: "/tmp/shared"},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := getSPCName(tc.namespace, tc.collection, tc.mountPath, tc.credentials)

			// Verify the name is lowercased
			if result != strings.ToLower(result) {
				t.Errorf("getSPCName() result should be lowercase: %v", result)
			}
			// Verify the name doesn't exceed 63 characters
			if len(result) > 63 {
				t.Errorf("getSPCName() result too long (%d chars): %v", len(result), result)
			}
			// Verify structure
			if !strings.HasPrefix(result, strings.ToLower(tc.namespace)+"-") {
				t.Errorf("getSPCName() should start with namespace: %v", result)
			}
			if !strings.HasSuffix(result, "-spc") {
				t.Errorf("getSPCName() should end with '-spc': %v", result)
			}
		})
	}
}

func TestGetSPCNameUniqueness(t *testing.T) {
	namespace := "ci-op-test123"

	testCases := []struct {
		collection  string
		mountPath   string
		credentials []api.CredentialReference
	}{
		{"collection1", "/tmp/cred1", []api.CredentialReference{
			{Name: "secret1", Collection: "collection1", MountPath: "/tmp/cred1"},
		}},
		{"collection1", "/tmp/cred2", []api.CredentialReference{
			{Name: "secret2", Collection: "collection1", MountPath: "/tmp/cred2"},
		}},
		{"collection2", "/tmp/cred1", []api.CredentialReference{
			{Name: "secret1", Collection: "collection2", MountPath: "/tmp/cred1"},
		}},
		{"collection2", "/tmp/cred2", []api.CredentialReference{
			{Name: "secret2", Collection: "collection2", MountPath: "/tmp/cred2"},
		}},
	}

	seen := make(map[string]bool)

	for _, tc := range testCases {
		result := getSPCName(namespace, tc.collection, tc.mountPath, tc.credentials)
		if seen[result] {
			t.Errorf("getSPCName() produced duplicate name %s for collection=%s, mountPath=%s", result, tc.collection, tc.mountPath)
		}
		seen[result] = true

		// Verify structure
		if !strings.HasPrefix(result, strings.ToLower(namespace)+"-") {
			t.Errorf("getSPCName() should start with namespace: %v", result)
		}
		if !strings.HasSuffix(result, "-spc") {
			t.Errorf("getSPCName() should end with '-spc': %v", result)
		}
		if len(result) > 63 {
			t.Errorf("getSPCName() result too long (%d chars): %v", len(result), result)
		}
	}
}

func TestGetSPCNameCollisionPrevention(t *testing.T) {
	namespace := "ci-op-test123"
	collection := "colours"
	mountPath := "/tmp/path"

	// Two different sets of credentials with same collection and mountPath
	credentials1 := []api.CredentialReference{
		{Name: "red", Collection: collection, MountPath: mountPath},
		{Name: "blue", Collection: collection, MountPath: mountPath},
	}

	credentials2 := []api.CredentialReference{
		{Name: "red", Collection: collection, MountPath: mountPath},
	}

	// They should get different SPC names
	spcName1 := getSPCName(namespace, collection, mountPath, credentials1)
	spcName2 := getSPCName(namespace, collection, mountPath, credentials2)

	if spcName1 == spcName2 {
		t.Errorf("Expected different SPC names for different credential sets, but got same name: %s", spcName1)
	}

	// But the same credentials should always give the same name
	spcName1Again := getSPCName(namespace, collection, mountPath, credentials1)
	if spcName1 != spcName1Again {
		t.Errorf("Expected same SPC name for same credentials, but got %s vs %s", spcName1, spcName1Again)
	}
}

func TestCSIVolumeName(t *testing.T) {
	testCases := []struct {
		name       string
		namespace  string
		collection string
		mountPath  string
		expected   string
	}{
		{
			name:       "simple case",
			namespace:  "test-ns",
			collection: "coll1",
			mountPath:  "/tmp/cred1",
			expected:   "test-ns-3b8b9081288110be",
		},
		{
			name:       "mount path with dots",
			namespace:  "test-ns",
			collection: "coll1",
			mountPath:  "/tmp/cred.with.dots",
			expected:   "test-ns-d0016e4cef6b95bc",
		},
		{
			name:       "mount path with underscores",
			namespace:  "test-ns",
			collection: "coll1",
			mountPath:  "/tmp/cred_with_underscores",
			expected:   "test-ns-b230621144162a62",
		},
		{
			name:       "long names stay within 63 char limit",
			namespace:  "long-namespace-name-within-limits",
			collection: "some-long-collection-name",
			mountPath:  "/long/mount/path/that/exceeds/kubernetes/limits",
			expected:   "long-namespace-name-within-limits-5208e4cada72c93e",
		},
		{
			name:       "long namespace triggers hash-only mode",
			namespace:  "namespace-that-is-just-long-enough-to-trigger-truncation",
			collection: "collection",
			mountPath:  "/tmp",
			expected:   "79bdf22088beba6ec83a0d18127b31df",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := getCSIVolumeName(tc.namespace, tc.collection, tc.mountPath)
			if result != tc.expected {
				t.Errorf("getCSIVolumeName() = %v, want %v", result, tc.expected)
			}
			// Also verify the length constraint
			if len(result) > 63 {
				t.Errorf("getCSIVolumeName() result too long (%d chars): %v", len(result), result)
			}
		})
	}
}

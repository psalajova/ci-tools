package gsmassmigration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/openshift/ci-tools/pkg/api"
)

func TestCredentialMigration(t *testing.T) {
	inputDir := "testdata/credentials-migration/input"
	expectedDir := "testdata/credentials-migration/expected"

	// Copy input to a temp dir so we don't modify test fixtures
	tmpDir := t.TempDir()
	if err := copyDir(inputDir, tmpDir); err != nil {
		t.Fatalf("Failed to copy input fixtures: %v", err)
	}

	// Simulate migration results: secret name -> (collection, group, target namespace)
	migrations := []MigrationResult{
		{SecretName: "loki-prod-collector-test-secret", Collection: "loki-ci", Group: "loki-prod-collector-test-secret", TargetNamespaces: "test-credentials"},
		{SecretName: "vsphere-ibmcloud-ci", Collection: "vsphere", Group: "ibmcloud/ci", TargetNamespaces: "test-credentials"},
		{SecretName: "ci-ibmcloud8", Collection: "vsphere-vmc", Group: "ibmcloud8", TargetNamespaces: "test-credentials"},
		{SecretName: "devqe-secrets", Collection: "openshift-qe", Group: "vsphere-devqe", TargetNamespaces: "test-credentials"},
		{SecretName: "telcov10n-ansible-group-all", Collection: "telcov10n-ci", Group: "ansible/ansible_group_all", TargetNamespaces: "test-credentials"},
		{SecretName: "telcov10n-ansible-group-kni-qe-92-masters", Collection: "telcov10n-ci", Group: "teams/network/clusters/kni-qe-92/ansible_group_masters", TargetNamespaces: "test-credentials"},
		{SecretName: "telcov10n-ansible-kni-qe-92-bastion", Collection: "telcov10n", Group: "kni-qe-92/bastion.kni-qe-92.telcoqe.eng.rdu2.dc.redhat.com", TargetNamespaces: "test-credentials"},
		{SecretName: "telcov10n-ansible-kni-qe-92-master0", Collection: "telcov10n-ci", Group: "teams/network/clusters/kni-qe-92/master0", TargetNamespaces: "test-credentials"},
		{SecretName: "telcov10n-ansible-hypervisors-helix88", Collection: "telcov10n-ci", Group: "teams/network/hypervisors/helix88.telcoqe.eng.rdu2.dc.redhat.com", TargetNamespaces: "test-credentials"},
		// Container test secrets: entries (no namespace)
		{SecretName: "my-test-secret", Collection: "my-test-collection", Group: "my-test-secret", TargetNamespaces: "ci"},
		{SecretName: "another-test-secret", Collection: "another-collection", Group: "another-test-secret", TargetNamespaces: "ci"},
	}

	// Step 1: Generate multi-source bundles (simulates Phase 3c in the real migration)
	// hypershift-agent-ibmz-credentials has 8 vault sources -> needs a bundle
	cache := &VaultCache{
		Secrets:      make(map[string]*CachedVaultSecret),
		ByTargetName: make(map[string][]*CachedVaultSecret),
		ByCollection: make(map[string][]*CachedVaultSecret),
	}
	for _, group := range []string{"abi-pull-secret", "brew-token", "httpd-vsi-ip", "httpd-vsi-key", "httpd-vsi-pub-key", "ibmcloud-apikey", "ibmcloud-zcomm-apikey", "quay-token"} {
		path := "kv/selfservice/hypershift-agent-ibmz-credentials/" + group
		secret := &CachedVaultSecret{
			Path:            path,
			Collection:      "hypershift-agent-ibmz-credentials",
			Group:           group,
			TargetName:      "hypershift-agent-ibmz-credentials",
			TargetNamespace: "test-credentials",
			Fields:          map[string]string{"key": "value"},
		}
		cache.Secrets[path] = secret
		cache.ByTargetName["hypershift-agent-ibmz-credentials"] = append(cache.ByTargetName["hypershift-agent-ibmz-credentials"], secret)
		cache.ByCollection["hypershift-agent-ibmz-credentials"] = append(cache.ByCollection["hypershift-agent-ibmz-credentials"], secret)
	}

	added, err := GenerateMultiSourceBundles(cache, tmpDir, false)
	if err != nil {
		t.Fatalf("GenerateMultiSourceBundles failed: %v", err)
	}
	t.Logf("Generated %d multi-source bundles", added) //rename to NumberOfAddedBundles

	// Step 2: Update credential stanzas (simulates Phase 4)
	_, err = UpdateCredentialStanzas(tmpDir, migrations, true, false)
	if err != nil {
		t.Fatalf("UpdateCredentialStanzas failed: %v", err)
	}

	err = filepath.Walk(expectedDir, func(expectedPath string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}

		relPath, err := filepath.Rel(expectedDir, expectedPath)
		if err != nil {
			return err
		}

		actualPath := filepath.Join(tmpDir, relPath)
		expected, err := os.ReadFile(expectedPath)
		if err != nil {
			t.Errorf("Failed to read expected file %s: %v", expectedPath, err)
			return nil
		}

		actual, err := os.ReadFile(actualPath)
		if err != nil {
			t.Errorf("Failed to read actual file %s: %v", actualPath, err)
			return nil
		}

		t.Logf("Comparing expected %s with actual %s", expectedPath, actualPath)
		if diff := cmp.Diff(string(expected), string(actual)); diff != "" {
			t.Errorf("File %s mismatch (-expected +actual):\n%s", relPath, diff)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("Failed to walk expected dir: %v", err)
	}
}

func TestUpdateSecretsNamespaceDisambiguation(t *testing.T) {
	testCases := []struct {
		name          string
		migrations    []MigrationResult
		secretName    string
		expectChanged bool
		expectedColl  string
		expectedGroup string
	}{
		{
			name: "prefers ci variant when same name synced to ci and test-credentials",
			migrations: []MigrationResult{
				{SecretName: "acm-sonarcloud-token", Collection: "ocm-secrets", Group: "acm-sonarcloud-token", TargetNamespaces: "test-credentials"},
				{SecretName: "acm-sonarcloud-token", Collection: "ocm-secrets", Group: "acm-sonarcloud-token-ci", TargetNamespaces: "ci"},
			},
			secretName:    "acm-sonarcloud-token",
			expectChanged: true,
			expectedColl:  "ocm-secrets",
			expectedGroup: "acm-sonarcloud-token-ci",
		},
		{
			name: "prefers ci variant across different collections",
			migrations: []MigrationResult{
				{SecretName: "cs-qe-credentials", Collection: "rosa-e2e", Group: "cs-qe-credentials", TargetNamespaces: "test-credentials"},
				{SecretName: "cs-qe-credentials", Collection: "ocm-fvt-credentials", Group: "cs-qe-credentials", TargetNamespaces: "ci"},
			},
			secretName:    "cs-qe-credentials",
			expectChanged: true,
			expectedColl:  "ocm-fvt-credentials",
			expectedGroup: "cs-qe-credentials",
		},
		{
			name: "falls back to single test-credentials variant when no ci variant exists",
			migrations: []MigrationResult{
				{SecretName: "ossm-github", Collection: "openshift-service-mesh", Group: "github-ossm-bot", TargetNamespaces: "test-credentials"},
			},
			secretName:    "ossm-github",
			expectChanged: true,
			expectedColl:  "openshift-service-mesh",
			expectedGroup: "github-ossm-bot",
		},
		{
			name:          "unmigrated when no matching vault secret",
			migrations:    nil,
			secretName:    "does-not-exist",
			expectChanged: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			credentialMap := buildCredentialMapping(tc.migrations)
			secrets := []*api.Secret{{Name: tc.secretName, MountPath: "/x"}}

			updates, changed := updateSecrets(secrets, credentialMap, sets.New[string](), sets.New[string](), "test.yaml")
			if changed != tc.expectChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.expectChanged)
			}
			if !tc.expectChanged {
				return
			}
			if len(updates) != 1 {
				t.Fatalf("expected 1 update, got %d", len(updates))
			}
			u := updates[0]
			if u.NewCollection != tc.expectedColl || u.NewGroup != tc.expectedGroup {
				t.Errorf("got %s/%s, want %s/%s", u.NewCollection, u.NewGroup, tc.expectedColl, tc.expectedGroup)
			}
			if !u.IsSecret {
				t.Errorf("expected IsSecret=true")
			}
		})
	}
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		dstPath := filepath.Join(dst, relPath)
		if info.IsDir() {
			return os.MkdirAll(dstPath, 0755)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		return os.WriteFile(dstPath, data, 0644)
	})
}

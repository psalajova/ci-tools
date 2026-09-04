package gsmassmigration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	added, err := GenerateMultiSourceBundles(cache, tmpDir, false, nil)
	if err != nil {
		t.Fatalf("GenerateMultiSourceBundles failed: %v", err)
	}
	t.Logf("Generated %d multi-source bundles", added) //rename to NumberOfAddedBundles

	// Step 2: Update credential stanzas (simulates Phase 4)
	_, err = UpdateCredentialStanzas(tmpDir, migrations, true, false, nil)
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

// TestCredentialMigrationTargetedScope guards the targeted-mode blast radius:
// GenerateMultiSourceBundles must only emit bundles for names in the filter, and
// UpdateCredentialStanzas with a TargetedScope must only rewrite the target repo's
// configs and only the migrated secrets (not arbitrary bundles) in the shared
// step-registry, leaving all other repos untouched.
func TestCredentialMigrationTargetedScope(t *testing.T) {
	writeFile := func(t *testing.T, root, rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(t *testing.T, root, rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	t.Run("GenerateMultiSourceBundles only emits filtered target names", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "core-services/ci-secret-bootstrap/gsm-config.yaml", "bundles: []\n")
		writeFile(t, root, "ci-operator/step-registry/cluster-profiles/cluster-profiles-config.yaml", "items: []\n")

		cache := &VaultCache{
			Secrets:      map[string]*CachedVaultSecret{},
			ByTargetName: map[string][]*CachedVaultSecret{},
			ByCollection: map[string][]*CachedVaultSecret{},
		}
		addMultiSource := func(target string) {
			for _, g := range []string{"a", "b"} {
				s := &CachedVaultSecret{Path: target + "/" + g, Collection: target, Group: g, TargetName: target, TargetNamespace: "test-credentials", Fields: map[string]string{"k": "v"}}
				cache.Secrets[s.Path] = s
				cache.ByTargetName[target] = append(cache.ByTargetName[target], s)
			}
		}
		addMultiSource("wanted-bundle")
		addMultiSource("unwanted-bundle")

		added, err := GenerateMultiSourceBundles(cache, root, false, sets.New[string]("wanted-bundle"))
		if err != nil {
			t.Fatalf("GenerateMultiSourceBundles failed: %v", err)
		}
		if added != 1 {
			t.Errorf("expected 1 bundle added, got %d", added)
		}
		out := read(t, root, "core-services/ci-secret-bootstrap/gsm-config.yaml")
		if !strings.Contains(out, "wanted-bundle") {
			t.Errorf("wanted-bundle missing from gsm-config.yaml:\n%s", out)
		}
		if strings.Contains(out, "unwanted-bundle") {
			t.Errorf("unwanted-bundle leaked into gsm-config.yaml despite filter:\n%s", out)
		}
	})

	t.Run("UpdateCredentialStanzas restricts rewrites to target repo and migrated secrets", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "core-services/ci-secret-bootstrap/gsm-config.yaml", "bundles:\n  - name: shared-bundle\n    gsm_secrets:\n        - collection: c\n          group: g\n")

		configFor := func(org, repo string) string {
			return fmt.Sprintf(`build_root:
  image_stream_tag:
    name: release
    namespace: openshift
    tag: rhel-9
resources:
  '*':
    requests:
      cpu: 100m
      memory: 200Mi
tests:
- as: t
  commands: make test
  container:
    from: src
  secrets:
  - mount_path: /a
    name: migrated-secret
  - mount_path: /b
    name: shared-bundle
zz_generated_metadata:
  branch: master
  org: %s
  repo: %s
`, org, repo)
		}
		targetRel := "ci-operator/config/targetorg/targetrepo/targetorg-targetrepo-master.yaml"
		otherRel := "ci-operator/config/otherorg/otherrepo/otherorg-otherrepo-master.yaml"
		writeFile(t, root, targetRel, configFor("targetorg", "targetrepo"))
		writeFile(t, root, otherRel, configFor("otherorg", "otherrepo"))

		refRel := "ci-operator/step-registry/my/ref/my-ref-ref.yaml"
		writeFile(t, root, refRel, `ref:
  as: my-ref
  from: src
  commands: my-ref-commands.sh
  credentials:
  - mount_path: /x
    name: migrated-secret
    namespace: test-credentials
  - mount_path: /y
    name: shared-bundle
    namespace: test-credentials
  documentation: doc
`)
		writeFile(t, root, "ci-operator/step-registry/my/ref/my-ref-commands.sh", "echo hi\n")

		migrations := []MigrationResult{
			{SecretName: "migrated-secret", Collection: "mycoll", Group: "mygroup", TargetNamespaces: "test-credentials,ci"},
		}

		scope := &TargetedScope{ConfigSubdir: "targetorg/targetrepo"}
		if _, err := UpdateCredentialStanzas(root, migrations, true, false, scope); err != nil {
			t.Fatalf("UpdateCredentialStanzas failed: %v", err)
		}

		target := read(t, root, targetRel)
		if !strings.Contains(target, "collection: mycoll") || !strings.Contains(target, "group: mygroup") {
			t.Errorf("target config: migrated-secret not converted to collection/group:\n%s", target)
		}
		if !strings.Contains(target, "bundle: shared-bundle") {
			t.Errorf("target config: shared-bundle not converted to bundle:\n%s", target)
		}

		if got := read(t, root, otherRel); got != configFor("otherorg", "otherrepo") {
			t.Errorf("other repo config must be untouched in targeted mode, got:\n%s", got)
		}

		ref := read(t, root, refRel)
		if !strings.Contains(ref, "collection: mycoll") {
			t.Errorf("step-registry: migrated-secret should be converted:\n%s", ref)
		}
		if strings.Contains(ref, "bundle: shared-bundle") {
			t.Errorf("step-registry: shared-bundle must NOT be converted in targeted mode:\n%s", ref)
		}
		if !strings.Contains(ref, "name: shared-bundle") {
			t.Errorf("step-registry: shared-bundle reference must remain unchanged:\n%s", ref)
		}
	})
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

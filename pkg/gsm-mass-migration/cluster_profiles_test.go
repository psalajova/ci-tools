package gsmassmigration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestClusterProfileMigration(t *testing.T) {
	inputDir := "testdata/cluster-profile-migration/input"
	expectedDir := "testdata/cluster-profile-migration/expected"

	tmpDir := t.TempDir()
	if err := copyDir(inputDir, tmpDir); err != nil {
		t.Fatalf("Failed to copy input fixtures: %v", err)
	}

	// Build fake vault cache with DPTP secrets referenced in _config.yaml
	cache := &VaultCache{
		Secrets:      make(map[string]*CachedVaultSecret),
		ByTargetName: make(map[string][]*CachedVaultSecret),
		ByCollection: make(map[string][]*CachedVaultSecret),
	}

	dptpSecrets := []struct {
		group  string
		fields map[string]string
	}{
		{"build_farm", map[string]string{
			"token_image-puller_app.ci_reg_auth_value.txt": "value",
		}},
		{"cloud.openshift.com-pull-secret", map[string]string{
			"auth": "value", "email": "value",
		}},
		{"quay.io-pull-secret", map[string]string{
			"auth": "value", "email": "value",
		}},
		{"quayio-ci-read-only-robot", map[string]string{
			"auth": "value",
		}},
		{"registry.connect.redhat.com-pull-secret", map[string]string{
			"auth": "value", "email": "value",
		}},
		{"registry.redhat.io-pull-secret", map[string]string{
			"auth": "value", "email": "value",
		}},
		{"cluster-bot-osd-ephemeral", map[string]string{
			".awscred":       "value",
			"aws-account-id": "value",
			"ocm-developer-productivity-staging.user":  "value",
			"ocm-developer-productivity-staging.token": "value",
		}},
		{"openshift-ci-aws-credentials", map[string]string{
			".awscred": "secret value",
		}},
		{"vsphere-credentials", map[string]string{
			"vmc.secret.auto.tfvars": "another value",
		}},
		{"openstack-ppc64le", map[string]string{
			"ca-cert.pem":    "value",
			"clouds.yaml":    "value",
			"ssh-privatekey": "value",
			"ssh-publickey":  "value",
		}},
	}

	for _, ds := range dptpSecrets {
		path := fmt.Sprintf("kv/dptp/%s", ds.group)
		secret := &CachedVaultSecret{
			Path:       path,
			Collection: "test-platform-infra",
			Group:      ds.group,
			Fields:     ds.fields,
		}
		cache.Secrets[path] = secret
		cache.ByCollection["test-platform-infra"] = append(
			cache.ByCollection["test-platform-infra"], secret)
	}

	// Add user vault secrets for a profile NOT in _config.yaml (user-secrets-only)
	// ibmcloud-qe-2 is a registered profile, default secret name: cluster-secrets-ibmcloud-qe-2
	userSecrets := []*CachedVaultSecret{
		// V5: user-secret-only profile (ibmcloud-qe-2, not in _config.yaml)
		{
			Path:            "kv/selfservice/openshift-qe/ibmcloud-qe-2",
			Collection:      "openshift-qe",
			Group:           "ibmcloud-qe-2",
			Fields:          map[string]string{"key1": "value"},
			TargetName:      "cluster-secrets-ibmcloud-qe-2",
			TargetNamespace: "ci",
		},
		{
			Path:            "kv/selfservice/openshift-qe/other-credentials",
			Collection:      "openshift-qe",
			Group:           "other-credentials",
			Fields:          map[string]string{"key2": "value"},
			TargetName:      "cluster-secrets-ibmcloud-qe-2",
			TargetNamespace: "ci",
		},
		// V4: user secret targeting osd-ephemeral, namespace matches (ci) -- should be included
		{
			Path:            "kv/selfservice/osd-team/osd-extra-creds",
			Collection:      "osd-team",
			Group:           "osd-extra-creds",
			Fields:          map[string]string{"api-key": "value"},
			TargetName:      "cluster-secrets-osd-ephemeral",
			TargetNamespace: "ci",
		},
		// V4: comma-separated namespace, one part matches (ci) -- should be included
		{
			Path:            "kv/selfservice/osd-team/osd-shared-creds",
			Collection:      "osd-team",
			Group:           "osd-shared-creds",
			Fields:          map[string]string{"token": "value"},
			TargetName:      "cluster-secrets-osd-ephemeral",
			TargetNamespace: "ci,test-credentials",
		},
		// V4: namespace does NOT match any cp ns (ci) -- should be excluded
		{
			Path:            "kv/selfservice/osd-team/osd-unrelated",
			Collection:      "osd-team",
			Group:           "osd-unrelated",
			Fields:          map[string]string{"token": "value"},
			TargetName:      "cluster-secrets-osd-ephemeral",
			TargetNamespace: "kube-system",
		},
		{
			Path:            "kv/selfservice/c/g",
			Collection:      "c",
			Group:           "g",
			Fields:          map[string]string{"token": "value"},
			TargetName:      "cluster-secrets-openstack-vexxhost",
			TargetNamespace: "some-random-ns",
		},
	}
	for _, s := range userSecrets {
		cache.Secrets[s.Path] = s
		cache.ByTargetName[s.TargetName] = append(cache.ByTargetName[s.TargetName], s)
	}

	// Step 1: Extract cluster profiles from _config.yaml and cluster-profiles-config.yaml
	clusterProfiles, _, _, err := ExtractClusterProfiles(tmpDir)
	if err != nil {
		t.Fatalf("ExtractClusterProfiles failed: %v", err)
	}

	// Step 1b: Discover user-secret-only profiles from vault cache
	userSecretOnlyProfiles, err := DiscoverUserSecretOnlyProfiles(tmpDir, cache, clusterProfiles)
	if err != nil {
		t.Fatalf("DiscoverUserSecretOnlyProfiles failed: %v", err)
	}
	clusterProfiles = append(clusterProfiles, userSecretOnlyProfiles...)

	// Step 2: Convert cache to VaultSecretData for bundle generator
	var allVaultSecrets []VaultSecretData
	for _, cached := range cache.Secrets {
		allVaultSecrets = append(allVaultSecrets, VaultSecretData{
			Path:            cached.Path,
			Collection:      cached.Collection,
			Group:           cached.Group,
			TargetName:      cached.TargetName,
			TargetNamespace: cached.TargetNamespace,
			IsEmpty:         cached.IsEmpty,
			IsPlaceholder:   cached.IsPlaceholder,
		})
	}

	// Step 3: Generate bundles
	result, err := GenerateBundles(clusterProfiles, allVaultSecrets)
	if err != nil {
		t.Fatalf("GenerateBundles failed: %v", err)
	}
	if len(result.Errors) > 0 {
		t.Fatalf("Bundle generation errors: %v", result.Errors)
	}

	// Step 4: Collect migrated profile names
	var migratedProfiles []string
	for _, bundle := range result.Bundles {
		migratedProfiles = append(migratedProfiles, bundle.Name)
	}

	// Step 5: Update config files
	_, err = UpdateConfigFiles(tmpDir, migratedProfiles, result.Bundles, false)
	if err != nil {
		t.Fatalf("UpdateConfigFiles failed: %v", err)
	}

	// Step 6: Compare output against expected
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

		if diff := cmp.Diff(string(expected), string(actual)); diff != "" {
			t.Errorf("File %s mismatch (-expected +actual):\n%s", relPath, diff)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("Failed to walk expected dir: %v", err)
	}
}

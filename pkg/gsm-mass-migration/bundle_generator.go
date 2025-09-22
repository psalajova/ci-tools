package gsmassmigration

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/openshift/ci-tools/pkg/api"
	secretsAPI "github.com/openshift/ci-tools/pkg/api/secretbootstrap"
	gsmvalidation "github.com/openshift/ci-tools/pkg/gsm-validation"
)

// BundleGenerationResult contains the results of bundle generation
type BundleGenerationResult struct {
	Bundles         []api.GSMBundle
	Warnings        []string
	Errors          []string
	SkippedProfiles []string // Profiles that couldn't be converted
}

// GenerateBundles converts _config.yaml cluster profiles to gsm-config.yaml bundles
func GenerateBundles(
	clusterProfiles []ClusterProfileSecret,
	vaultSecrets []VaultSecretData,
) (*BundleGenerationResult, error) {
	result := &BundleGenerationResult{
		Bundles:  []api.GSMBundle{},
		Warnings: []string{},
		Errors:   []string{},
	}

	// Build index of vault secrets by secretsync/target-name
	userSecretsByTarget := buildUserSecretIndex(vaultSecrets)

	// Process each cluster profile definition
	for _, profile := range clusterProfiles {
		bundle, err := generateBundle(
			profile,
			userSecretsByTarget,
		)

		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", profile.Name, err))
			result.SkippedProfiles = append(result.SkippedProfiles, profile.Name)
			continue
		}

		if bundle != nil {
			result.Bundles = append(result.Bundles, *bundle)
		}
	}

	return result, nil
}

// buildUserSecretIndex creates a map of secretsync/target-name to vault secrets
func buildUserSecretIndex(vaultSecrets []VaultSecretData) map[string][]VaultSecretData {
	index := make(map[string][]VaultSecretData)

	for _, secret := range vaultSecrets {
		if secret.TargetName != "" {
			index[secret.TargetName] = append(index[secret.TargetName], secret)
		}
	}

	return index
}

// generateBundle creates a GSMBundle for a single cluster profile
func generateBundle(
	profile ClusterProfileSecret,
	userSecretsByTarget map[string][]VaultSecretData,
) (*api.GSMBundle, error) {
	bundle := &api.GSMBundle{
		Name:          profile.Name,
		SyncToCluster: true,
		Targets:       convertTargets(profile.Targets),
	}

	// Generate dockerconfig section from DPTP fields (only if present)
	if len(profile.DockerConfigItems) > 0 {
		dockerconfig, _, err := generateDockerConfig(profile.DockerConfigKey, profile.DockerConfigItems)
		if err != nil {
			return nil, fmt.Errorf("failed to generate dockerconfig: %w", err)
		}
		bundle.DockerConfig = dockerconfig
	}

	// Add DPTP non-dockerconfig fields to gsm_secrets (always, independent of user secrets)
	if len(profile.DPTPFields) > 0 {
		type fieldRef struct {
			vaultField   string
			k8sFieldName string
		}
		itemToFields := make(map[string][]fieldRef)
		for _, dptpField := range profile.DPTPFields {
			itemName := strings.TrimPrefix(dptpField.ItemName, "dptp/")
			itemToFields[itemName] = append(itemToFields[itemName], fieldRef{
				vaultField:   dptpField.VaultFieldName,
				k8sFieldName: dptpField.K8sFieldName,
			})
		}
		itemNames := make([]string, 0, len(itemToFields))
		for itemName := range itemToFields {
			itemNames = append(itemNames, itemName)
		}
		sort.Strings(itemNames)
		for _, itemName := range itemNames {
			fields := itemToFields[itemName]
			var fieldEntries []api.FieldEntry
			for _, f := range fields {
				normalizedVaultField := gsmvalidation.NormalizeName(f.vaultField)
				entry := api.FieldEntry{Name: normalizedVaultField}
				if f.k8sFieldName != normalizedVaultField {
					entry.As = f.k8sFieldName
				}
				fieldEntries = append(fieldEntries, entry)
			}
			sort.Slice(fieldEntries, func(i, j int) bool {
				return fieldEntries[i].Name < fieldEntries[j].Name
			})
			bundle.GSMSecrets = append(bundle.GSMSecrets, api.GSMSecretRef{
				Collection: "test-platform-infra",
				Group:      gsmvalidation.NormalizeName(itemName),
				Fields:     fieldEntries,
			})
		}
	}

	// Add user secrets to gsm_secrets (only if their target namespace matches a profile target)
	profileNamespaces := make(map[string]bool)
	for _, t := range profile.Targets {
		for _, ns := range strings.Split(t.Namespace, ",") {
			ns = strings.TrimSpace(ns)
			if ns != "" {
				profileNamespaces[ns] = true
			}
		}
	}

	candidates := userSecretsByTarget[profile.Name]
	var userGSMSecrets []api.GSMSecretRef
	for _, userSecret := range candidates {
		if userSecret.TargetNamespace != "" && !namespacesOverlap(userSecret.TargetNamespace, profileNamespaces) {
			logrus.Debugf("Skipping user secret %s/%s for %s: target namespace %q does not match profile namespaces",
				userSecret.Collection, userSecret.Group, profile.Name, userSecret.TargetNamespace)
			continue
		}
		userGSMSecrets = append(userGSMSecrets, api.GSMSecretRef{
			Collection: gsmvalidation.NormalizeName(userSecret.Collection),
			Group:      gsmvalidation.NormalizeName(userSecret.Group),
		})
	}
	sort.Slice(userGSMSecrets, func(i, j int) bool {
		if userGSMSecrets[i].Collection != userGSMSecrets[j].Collection {
			return userGSMSecrets[i].Collection < userGSMSecrets[j].Collection
		}
		return userGSMSecrets[i].Group < userGSMSecrets[j].Group
	})
	bundle.GSMSecrets = append(bundle.GSMSecrets, userGSMSecrets...)

	if len(profile.DPTPFields) == 0 && len(userGSMSecrets) == 0 {
		logrus.Debugf("Cluster profile %s has only DPTP dockerconfig", profile.Name)
	}

	return bundle, nil
}

// generateDockerConfig builds the dockerconfig section from cluster profile registry items
// Returns the dockerconfig spec and the key name it will create in the K8s secret
func generateDockerConfig(keyName string, items []secretsAPI.DockerConfigJSONData) (*api.DockerConfigSpec, string, error) {
	if len(items) == 0 {
		return nil, "", fmt.Errorf("no dockerconfig items found")
	}

	var registries []api.RegistryAuthData

	for _, item := range items {
		// Strip dptp/ prefix added by secretbootstrap.resolve(), then normalize
		groupName := strings.TrimPrefix(item.Item, "dptp/")
		normalizedGroup := gsmvalidation.NormalizeName(groupName)
		normalizedAuthField := gsmvalidation.NormalizeName(item.AuthField)

		registry := api.RegistryAuthData{
			Group:       normalizedGroup,
			RegistryURL: item.RegistryURL,
			AuthField:   normalizedAuthField,
		}

		// Email field is optional
		if item.EmailField != "" {
			registry.EmailField = gsmvalidation.NormalizeName(item.EmailField)
		}

		registries = append(registries, registry)
	}

	// Default key name if not specified
	if keyName == "" {
		keyName = "pull-secret"
	}

	return &api.DockerConfigSpec{
		As:         keyName,
		Registries: registries,
	}, keyName, nil
}

// convertTargets converts secretsAPI.SecretContext to api.TargetSpec.
// Deduplicates targets that were expanded by secretbootstrap.resolve().
func convertTargets(targets []secretsAPI.SecretContext) []api.TargetSpec {
	seen := make(map[string]bool)
	var result []api.TargetSpec
	for _, target := range targets {
		// Dedup key based on what we OUTPUT, not the expanded input.
		// When cluster_groups is set, ignore the expanded Cluster field.
		var key string
		if len(target.ClusterGroups) > 0 {
			key = "cg:" + strings.Join(target.ClusterGroups, ",") + ":" + target.Namespace
		} else {
			key = "c:" + target.Cluster + ":" + target.Namespace
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		spec := api.TargetSpec{Namespace: target.Namespace}
		if len(target.ClusterGroups) > 0 {
			spec.ClusterGroups = target.ClusterGroups
		} else if target.Cluster != "" {
			spec.Cluster = target.Cluster
		}
		result = append(result, spec)
	}
	return result
}

// namespacesOverlap checks if a comma-separated namespace string (e.g. "test-credentials,ci")
// has any overlap with the given namespace set.
func namespacesOverlap(targetNamespace string, namespaces map[string]bool) bool {
	for _, ns := range strings.Split(targetNamespace, ",") {
		if namespaces[strings.TrimSpace(ns)] {
			return true
		}
	}
	return false
}

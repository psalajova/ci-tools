package gsmassmigration

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/openshift/ci-tools/pkg/api"
	gsmvalidation "github.com/openshift/ci-tools/pkg/gsm-validation"
)

// GenerateMultiSourceBundles finds non-cluster-profile secrets that are assembled
// from multiple Vault sources (via secretsync/target-name) and generates bundles
// for them. These secrets can't be migrated to a simple collection/group credential
// stanza because they span multiple groups.
//
// Returns the generated bundles and the set of target names that got bundles
// (so the credential migration can convert them to bundle: references).
func GenerateMultiSourceBundles(
	cache *VaultCache,
	releaseRepoPath string,
	dryRun bool,
) (int, error) {
	// Build set of cluster profile secret names to exclude (handled separately)
	cpNames, _, _, err := buildCompleteClusterProfileSecretMap(releaseRepoPath)
	if err != nil {
		return 0, fmt.Errorf("failed to build cluster profile map: %w", err)
	}

	// Group by (TargetName, TargetNamespace) -- same target name with different
	// namespaces are separate secrets, not multiple sources for one secret.
	type targetKey struct{ name, namespace string }
	byTarget := make(map[targetKey][]*CachedVaultSecret)
	for targetName, secrets := range cache.ByTargetName {
		if cpNames[targetName] {
			continue
		}
		for _, s := range secrets {
			if s.IsEmpty || s.IsPlaceholder {
				continue
			}
			// Split comma-separated namespaces into individual keys
			namespaces := strings.Split(s.TargetNamespace, ",")
			if len(namespaces) == 0 || (len(namespaces) == 1 && namespaces[0] == "") {
				namespaces = []string{"ci"}
			}
			for _, ns := range namespaces {
				ns = strings.TrimSpace(ns)
				if ns != "" {
					key := targetKey{name: targetName, namespace: ns}
					byTarget[key] = append(byTarget[key], s)
				}
			}
		}
	}

	// Only create bundles for (name, namespace) pairs with multiple sources
	var bundles []api.GSMBundle
	for key, validSecrets := range byTarget {
		if len(validSecrets) < 2 {
			continue
		}

		var gsmSecrets []api.GSMSecretRef
		for _, s := range validSecrets {
			gsmSecrets = append(gsmSecrets, api.GSMSecretRef{
				Collection: gsmvalidation.NormalizeName(s.Collection),
				Group:      gsmvalidation.NormalizeName(s.Group),
			})
		}

		bundles = append(bundles, api.GSMBundle{
			Name:       key.name,
			GSMSecrets: gsmSecrets,
		})

		logrus.Debugf("Generated multi-source bundle %s (namespace=%s, %d vault sources)", key.name, key.namespace, len(validSecrets))
	}

	if len(bundles) == 0 {
		logrus.Info("No multi-source credential bundles to generate")
		return 0, nil
	}

	logrus.Infof("Generated %d multi-source credential bundles", len(bundles))

	// Append to gsm-config.yaml
	gsmConfigPath := filepath.Join(releaseRepoPath, "core-services/ci-secret-bootstrap/gsm-config.yaml")
	added, err := appendBundlesToGSMConfig(gsmConfigPath, bundles, dryRun)
	if err != nil {
		return 0, fmt.Errorf("failed to append multi-source bundles to gsm-config.yaml: %w", err)
	}

	return added, nil
}

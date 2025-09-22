package gsmassmigration

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"
	"sigs.k8s.io/yaml"

	"github.com/openshift/ci-tools/pkg/api"
	secretbootstrap "github.com/openshift/ci-tools/pkg/api/secretbootstrap"
)

// DPTPFieldRef represents a non-dockerconfig DPTP field reference from _config.yaml
type DPTPFieldRef struct {
	K8sFieldName   string // Key in K8s secret (e.g., "sso-client-id")
	VaultFieldName string // Field within the Vault item (e.g., "ocm-developer-productivity-staging.user")
	ItemName       string // Vault item (e.g., "cluster-bot-osd-ephemeral")
}

// ClusterProfileSecret represents a cluster profile secret from _config.yaml
type ClusterProfileSecret struct {
	Name              string
	DockerConfigKey   string
	DockerConfigItems []secretbootstrap.DockerConfigJSONData
	DPTPFields        []DPTPFieldRef
	Targets           []secretbootstrap.SecretContext
}

// buildCompleteClusterProfileSecretMap builds a complete map of all valid cluster profile secret names
// from cluster-profiles-config.yaml (the single source of truth for cluster profiles).
// TODO: rework for new ClusterProfiles API (cluster profiles moved from ci-tools to cluster-profiles-config.yaml)
func buildCompleteClusterProfileSecretMap(releaseRepoPath string) (map[string]bool, error) {
	configPath := filepath.Join(releaseRepoPath, "ci-operator/step-registry/cluster-profiles/cluster-profiles-config.yaml")
	content, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read cluster-profiles-config.yaml: %w", err)
	}

	var profilesConfig api.ClusterProfiles
	if err := yaml.Unmarshal(content, &profilesConfig); err != nil {
		return nil, fmt.Errorf("failed to parse cluster-profiles-config.yaml: %w", err)
	}

	secretNames := make(map[string]bool)
	for _, profile := range profilesConfig.Items {
		if profile.IsASet() {
			continue
		}
		secretName := profile.Secret
		if secretName == "" {
			secretName = "cluster-secrets-" + profile.Name
		}
		secretNames[secretName] = true
	}

	logrus.Debugf("Built complete cluster profile secret map with %d entries", len(secretNames))
	return secretNames, nil
}

// ExtractClusterProfiles reads _config.yaml and extracts cluster profile secrets.
// Returns only secrets that are actual cluster profiles (have dockerconfigJSON and are in the complete profile list).
// Also returns cluster_groups map and DPTP items map.
func ExtractClusterProfiles(releaseRepoPath string) ([]ClusterProfileSecret, map[string][]string, map[string]bool, error) {
	configPath := filepath.Join(releaseRepoPath, "core-services/ci-secret-bootstrap/_config.yaml")

	logrus.Infof("Reading cluster profiles from %s", configPath)

	validSecretNames, err := buildCompleteClusterProfileSecretMap(releaseRepoPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to build cluster profile secret map: %w", err)
	}

	var config secretbootstrap.Config
	if err := secretbootstrap.LoadConfigFromFile(configPath, &config); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to load _config.yaml: %w", err)
	}

	var profiles []ClusterProfileSecret
	dptpItems := make(map[string]bool)

	for _, secretDef := range config.Secrets {
		if secretDef.From == nil || len(secretDef.To) == 0 {
			continue
		}

		// Group targets by secret name (one from block can create many cluster profiles)
		targetsByName := make(map[string][]secretbootstrap.SecretContext)
		for _, to := range secretDef.To {
			targetsByName[to.Name] = append(targetsByName[to.Name], to)
		}

		// Check if ANY target is a valid cluster profile in the ci namespace.
		// Cluster profile secrets always target the ci namespace; entries targeting
		// only other namespaces are regular secrets that happen to share a profile name.
		hasClusterProfile := false
		for name, targets := range targetsByName {
			if !validSecretNames[name] {
				continue
			}
			for _, t := range targets {
				if t.Namespace == "ci" {
					hasClusterProfile = true
					break
				}
			}
			if hasClusterProfile {
				break
			}
		}
		if !hasClusterProfile {
			continue
		}

		// Parse the shared "from" section once
		var dockerConfigKey string
		var dockerConfigData []secretbootstrap.DockerConfigJSONData
		var dptpFields []DPTPFieldRef

		for k8sFieldName, fieldDef := range secretDef.From {
			if fieldDef.DockerConfigJSONData != nil {
				dockerConfigKey = k8sFieldName
				dockerConfigData = fieldDef.DockerConfigJSONData
			} else if fieldDef.Item != "" {
				vaultField := fieldDef.Field
				if vaultField == "" {
					vaultField = k8sFieldName
				}
				dptpFields = append(dptpFields, DPTPFieldRef{
					K8sFieldName:   k8sFieldName,
					VaultFieldName: vaultField,
					ItemName:       fieldDef.Item,
				})
			}
		}

		if dockerConfigKey == "" {
			continue
		}

		// Track DPTP items (shared across all profiles from this block)
		for _, item := range dockerConfigData {
			dptpItems[item.Item] = true
		}
		for _, field := range dptpFields {
			dptpItems[field.ItemName] = true
		}

		// Create a separate ClusterProfileSecret for each valid target name
		// Sort names for deterministic output
		sortedNames := make([]string, 0, len(targetsByName))
		for name := range targetsByName {
			sortedNames = append(sortedNames, name)
		}
		sort.Strings(sortedNames)
		for _, name := range sortedNames {
			targets := targetsByName[name]
			if !validSecretNames[name] {
				logrus.Tracef("Skipping secret %s because it is not a cp secret name", name)
				continue
			}
			hasCINamespace := false
			for _, t := range targets {
				if t.Namespace == "ci" {
					hasCINamespace = true
					break
				}
			}
			if !hasCINamespace {
				logrus.Tracef("Skipping secret %s because it does not target the ci namespace", name)
				continue
			}
			profiles = append(profiles, ClusterProfileSecret{
				Name:              name,
				DockerConfigKey:   dockerConfigKey,
				DockerConfigItems: dockerConfigData,
				DPTPFields:        dptpFields,
				Targets:           targets,
			})
		}
	}

	// Log DPTP items for debugging
	if len(dptpItems) > 0 {
		itemsList := make([]string, 0, len(dptpItems))
		for item := range dptpItems {
			itemsList = append(itemsList, item)
		}
		logrus.Debugf("DPTP items extracted from _config.yaml: %v", itemsList)
	}

	logrus.Infof("Found %d cluster profiles, %d unique DPTP items", len(profiles), len(dptpItems))
	return profiles, config.ClusterGroups, dptpItems, nil
}

// DiscoverUserSecretOnlyProfiles finds cluster profiles that have no _config.yaml entry
// but are assembled entirely from user selfservice Vault secrets (via secretsync/target-name).
// These profiles need bundles with only gsm_secrets (no dockerconfig).
func DiscoverUserSecretOnlyProfiles(releaseRepoPath string, cache *VaultCache, alreadyFound []ClusterProfileSecret) ([]ClusterProfileSecret, error) {
	validSecretNames, err := buildCompleteClusterProfileSecretMap(releaseRepoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to build cluster profile secret map: %w", err)
	}

	alreadyFoundNames := make(map[string]bool)
	for _, p := range alreadyFound {
		alreadyFoundNames[p.Name] = true
	}

	var discovered []ClusterProfileSecret

	for targetName, secrets := range cache.ByTargetName {
		if !validSecretNames[targetName] || alreadyFoundNames[targetName] {
			continue
		}

		// Filter to non-empty, non-placeholder secrets
		var validSecrets []*CachedVaultSecret
		for _, s := range secrets {
			if !s.IsEmpty && !s.IsPlaceholder {
				validSecrets = append(validSecrets, s)
			}
		}
		if len(validSecrets) == 0 {
			continue
		}

		// Derive targets from secret metadata
		// Collect all unique namespaces from all secrets targeting this profile
		namespaces := make(map[string]bool)
		for _, s := range validSecrets {
			if s.TargetNamespace != "" {
				for _, ns := range strings.Split(s.TargetNamespace, ",") {
					ns = strings.TrimSpace(ns)
					if ns != "" {
						namespaces[ns] = true
					}
				}
			}
		}
		if len(namespaces) == 0 {
			namespaces["ci"] = true
		}

		// Cluster profile secrets must target the ci namespace
		if !namespaces["ci"] {
			logrus.Debugf("Skipping user-secret-only profile %s: does not target ci namespace", targetName)
			continue
		}

		// Check if any secret specifies target-clusters
		targetClusters := ""
		for _, s := range validSecrets {
			if s.TargetClusters != "" {
				targetClusters = s.TargetClusters
				break
			}
		}

		var targets []secretbootstrap.SecretContext
		for ns := range namespaces {
			if targetClusters != "" {
				for _, cluster := range strings.Split(targetClusters, ",") {
					cluster = strings.TrimSpace(cluster)
					if cluster != "" {
						targets = append(targets, secretbootstrap.SecretContext{
							Cluster:   cluster,
							Namespace: ns,
						})
					}
				}
			} else {
				targets = append(targets, secretbootstrap.SecretContext{
					ClusterGroups: []string{"user_secrets_target_clusters"},
					Namespace:     ns,
				})
			}
		}

		discovered = append(discovered, ClusterProfileSecret{
			Name:    targetName,
			Targets: targets,
		})

		logrus.Debugf("Discovered user-secret-only profile %s (%d vault sources, %d targets)",
			targetName, len(validSecrets), len(targets))
	}

	logrus.Infof("Discovered %d user-secret-only cluster profiles (not in _config.yaml)", len(discovered))
	return discovered, nil
}

// VaultSecretData represents the minimal data we need from a vault secret for migration
type VaultSecretData struct {
	Path            string
	Collection      string
	Group           string
	TargetName      string // From secretsync/target-name
	TargetNamespace string // From secretsync/target-namespace
	IsEmpty         bool
	IsPlaceholder   bool
}

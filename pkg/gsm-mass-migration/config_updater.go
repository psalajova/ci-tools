package gsmassmigration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"

	"github.com/openshift/ci-tools/pkg/api"
	apibootstrap "github.com/openshift/ci-tools/pkg/api/secretbootstrap"
)

// UpdateConfigFiles updates _config.yaml and gsm-config.yaml
// - Removes migrated cluster profiles from _config.yaml
// - Adds generated bundles to gsm-config.yaml (or creates it)
// ConfigUpdateResult holds counts from config file updates
type ConfigUpdateResult struct {
	BundlesAdded         int
	ConfigEntriesRemoved int
}

func UpdateConfigFiles(
	releaseRepoPath string,
	migratedProfiles []string,
	newBundles []api.GSMBundle,
	dryRun bool,
) (*ConfigUpdateResult, error) {
	configPath := filepath.Join(releaseRepoPath, "core-services/ci-secret-bootstrap/_config.yaml")
	gsmConfigPath := filepath.Join(releaseRepoPath, "core-services/ci-secret-bootstrap/gsm-config.yaml")

	// 1. Add bundles to gsm-config.yaml first (safe: additive, no breakage)
	bundlesAdded, err := appendBundlesToGSMConfig(gsmConfigPath, newBundles, dryRun)
	if err != nil {
		return nil, fmt.Errorf("failed to update gsm-config.yaml: %w", err)
	}

	// 2. Remove migrated profiles from _config.yaml (only after bundles are in place)
	entriesRemoved, err := removeClusterProfilesFromConfig(configPath, migratedProfiles, dryRun)
	if err != nil {
		return nil, fmt.Errorf("failed to update _config.yaml: %w", err)
	}

	return &ConfigUpdateResult{
		BundlesAdded:         bundlesAdded,
		ConfigEntriesRemoved: entriesRemoved,
	}, nil
}

// removeClusterProfilesFromConfig removes migrated cluster profiles from _config.yaml
func removeClusterProfilesFromConfig(configPath string, migratedProfiles []string, dryRun bool) (int, error) {
	logrus.Infof("Removing %d cluster profiles from %s", len(migratedProfiles), configPath)

	// Read current config
	var config apibootstrap.Config
	if err := apibootstrap.LoadConfigFromFile(configPath, &config); err != nil {
		return 0, fmt.Errorf("failed to load config: %w", err)
	}

	// Build set of migrated profile names for fast lookup
	migratedSet := sets.New(migratedProfiles...)

	// Filter out migrated profile targets, keeping non-migrated ones
	var remaining []apibootstrap.SecretConfig
	removedNames := sets.New[string]()

	for _, secretDef := range config.Secrets {
		// Check which to entries are migrated
		var keptTargets []apibootstrap.SecretContext
		for _, to := range secretDef.To {
			if migratedSet.Has(to.Name) {
				removedNames.Insert(to.Name)
			} else {
				keptTargets = append(keptTargets, to)
			}
		}

		if len(keptTargets) == len(secretDef.To) {
			// Nothing removed, keep as-is
			remaining = append(remaining, secretDef)
		} else if len(keptTargets) > 0 {
			// Some targets remain, keep the from block with remaining targets
			secretDef.To = keptTargets
			remaining = append(remaining, secretDef)
			logrus.Debugf("Kept %d non-migrated targets in shared from block", len(keptTargets))
		}
		// else: all targets removed, drop the entire block
	}

	removedCount := removedNames.Len()
	logrus.Infof("Removed %d config entries, %d definitions remaining", removedCount, len(remaining))

	// Update config
	config.Secrets = remaining

	// Write back
	if dryRun {
		logrus.Infof("[DRY-RUN] Would update %s", configPath)
		return removedCount, nil
	}

	if err := apibootstrap.SaveConfigToFile(configPath, &config); err != nil {
		return 0, fmt.Errorf("failed to save config: %w", err)
	}

	logrus.Infof("Updated %s", configPath)
	return removedCount, nil
}

// appendBundlesToGSMConfig appends new bundles to gsm-config.yaml.
// Preserves the existing file content exactly and appends new bundle entries as text
// at the end of the bundles: array. This avoids triggering resolve() which expands cluster_groups.
func appendBundlesToGSMConfig(gsmConfigPath string, newBundles []api.GSMBundle, dryRun bool) (int, error) {
	logrus.Infof("Adding bundles to %s", gsmConfigPath)

	// Read existing file as text
	existingContent, err := os.ReadFile(gsmConfigPath)
	if err != nil {
		return 0, fmt.Errorf("failed to read gsm-config.yaml: %w", err)
	}
	existingText := string(existingContent)

	// Extract existing bundle names for idempotency (simple text scan)
	existingBundleNames := extractBundleNamesFromText(existingText)

	// Filter out duplicates
	var bundlesToAdd []api.GSMBundle
	skippedCount := 0

	for _, bundle := range newBundles {
		if existingBundleNames.Has(bundle.Name) {
			logrus.Debugf("Bundle %s already exists in gsm-config.yaml, skipping", bundle.Name)
			skippedCount++
			continue
		}
		bundlesToAdd = append(bundlesToAdd, bundle)
	}

	if len(bundlesToAdd) == 0 {
		logrus.Infof("No new bundles to add (all %d bundles already exist)", skippedCount)
		return 0, nil
	}

	logrus.Infof("Adding %d new bundles (skipped %d duplicates)", len(bundlesToAdd), skippedCount)

	if dryRun {
		logrus.Infof("[DRY-RUN] Would add %d bundles to %s", len(bundlesToAdd), gsmConfigPath)
		for _, bundle := range bundlesToAdd {
			logrus.Debugf("[DRY-RUN] Would add bundle: %s", bundle.Name)
		}
		return len(bundlesToAdd), nil
	}

	// Marshal each new bundle and indent to match the existing bundles: array style
	var appendText strings.Builder
	for _, bundle := range bundlesToAdd {
		bundleYAML, err := yaml.Marshal(bundle)
		if err != nil {
			return 0, fmt.Errorf("failed to marshal bundle %s: %w", bundle.Name, err)
		}
		// yaml.Marshal produces "key: value\n..." for a struct.
		// Indent each line by 4 spaces and prefix first line with "  - " to make it
		// a list item under "bundles:".
		lines := strings.Split(strings.TrimRight(string(bundleYAML), "\n"), "\n")
		appendText.WriteString("\n")
		for i, line := range lines {
			if i == 0 {
				appendText.WriteString("  - " + line + "\n")
			} else {
				appendText.WriteString("    " + line + "\n")
			}
		}
	}

	// Append to existing file (bundles: is the last section)
	newContent := strings.TrimRight(existingText, "\n") + "\n" + appendText.String()

	if err := os.WriteFile(gsmConfigPath, []byte(newContent), 0644); err != nil {
		return 0, fmt.Errorf("failed to write gsm-config.yaml: %w", err)
	}

	logrus.Infof("Updated %s (added %d bundles)", gsmConfigPath, len(bundlesToAdd))
	return len(bundlesToAdd), nil
}

// extractBundleNamesFromText scans YAML text to find bundle names without parsing
func extractBundleNamesFromText(text string) sets.Set[string] {
	names := sets.New[string]()
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		// Bundle names appear as "- name: foo" (list items) or "name: foo"
		trimmed = strings.TrimPrefix(trimmed, "- ")
		if strings.HasPrefix(trimmed, "name:") {
			name := strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
			if name != "" {
				names.Insert(name)
			}
		}
	}
	return names
}

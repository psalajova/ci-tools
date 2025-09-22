package gsmassmigration

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"

	"github.com/openshift/ci-tools/pkg/group"
)

const dptpRoverGroup = "test-platform-gsm-secrets-owners"

type VaultCollectionsConfig struct {
	VaultCollections map[string]VaultCollectionEntry `json:"vault_collections" yaml:"vault_collections"`
}

type VaultCollectionEntry struct {
	Owners      []string `json:"owners" yaml:"owners"`
	RoverGroups []string `json:"rover-groups" yaml:"rover-groups"`
}

// UpdateRoverGroupsConfig reads vault-collections-owners.yaml and updates
// sync-rover-groups/_config.yaml with collection-to-group mappings.
// Collections with no rover groups assigned are added to the DPTP group.
func UpdateRoverGroupsConfig(releaseRepoPath, vaultCollectionsFile string, dryRun bool) error {
	// Load vault collections file
	data, err := os.ReadFile(vaultCollectionsFile)
	if err != nil {
		return fmt.Errorf("failed to read vault collections file: %w", err)
	}
	var vaultConfig VaultCollectionsConfig
	if err := yaml.Unmarshal(data, &vaultConfig); err != nil {
		return fmt.Errorf("failed to parse vault collections file: %w", err)
	}
	logrus.Infof("Loaded %d vault collections from %s", len(vaultConfig.VaultCollections), vaultCollectionsFile)

	// Load rover groups config
	configPath := filepath.Join(releaseRepoPath, "core-services/sync-rover-groups/_config.yaml")
	roverConfig, err := group.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load rover groups config: %w", err)
	}
	if roverConfig.Groups == nil {
		roverConfig.Groups = make(map[string]group.Target)
	}

	// Build collection→groups mapping
	newGroups := 0
	collectionsAdded := 0
	unownedCollections := 0

	// Sort collection names for deterministic output
	collectionNames := make([]string, 0, len(vaultConfig.VaultCollections))
	for name := range vaultConfig.VaultCollections {
		collectionNames = append(collectionNames, name)
	}
	sort.Strings(collectionNames)

	for _, collection := range collectionNames {
		entry := vaultConfig.VaultCollections[collection]
		roverGroups := entry.RoverGroups

		if len(roverGroups) == 0 {
			roverGroups = []string{dptpRoverGroup}
			unownedCollections++
			logrus.Debugf("Collection %s has no rover groups, assigning to %s", collection, dptpRoverGroup)
		}

		for _, roverGroup := range roverGroups {
			target, exists := roverConfig.Groups[roverGroup]
			if !exists {
				target = group.Target{}
				newGroups++
			}

			existing := sets.New(target.SecretCollections...)
			if existing.Has(collection) {
				continue
			}

			target.SecretCollections = append(target.SecretCollections, collection)
			sort.Strings(target.SecretCollections)
			roverConfig.Groups[roverGroup] = target
			collectionsAdded++
		}
	}

	logrus.Infof("Added %d collection mappings (%d unowned → %s), %d new groups",
		collectionsAdded, unownedCollections, dptpRoverGroup, newGroups)

	if collectionsAdded == 0 {
		logrus.Info("No changes needed")
		return nil
	}

	if dryRun {
		logrus.Infof("[DRY-RUN] Would update %s", configPath)
		return nil
	}

	rawYaml, err := yaml.Marshal(roverConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	if err := os.WriteFile(configPath, rawYaml, 0644); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	logrus.Infof("Updated %s", configPath)
	return nil
}

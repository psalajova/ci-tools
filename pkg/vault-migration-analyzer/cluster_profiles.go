package vaultmigrationanalyzer

import (
	"fmt"
	"path/filepath"

	"github.com/sirupsen/logrus"

	secretsAPI "github.com/openshift/ci-tools/pkg/api/secretbootstrap"
)

// AnalyzeClusterProfiles analyzes cluster profile definitions from _config.yaml
func AnalyzeClusterProfiles(releaseRepoPath string) (*ClusterProfileReport, error) {
	configPath := filepath.Join(releaseRepoPath, "core-services/ci-secret-bootstrap/_config.yaml")

	logrus.Infof("Parsing %s", configPath)

	var config secretsAPI.Config
	if err := secretsAPI.LoadConfigFromFile(configPath, &config); err != nil {
		return nil, fmt.Errorf("failed to load _config.yaml: %w", err)
	}

	report := &ClusterProfileReport{
		Definitions:  []ClusterProfileSecretDefinition{},
		ItemUsageMap: make(map[string]ItemUsage),
	}

	// Process each secret definition
	for _, secretDef := range config.Secrets {
		if secretDef.From == nil || len(secretDef.To) == 0 {
			continue
		}

		profile := ClusterProfileSecretDefinition{
			Name:              secretDef.To[0].Name,
			Items:             ClusterProfileItems{DPTP: []string{}, Selfservice: []string{}},
			Fields:            []FieldMapping{},
			Targets:           []ClusterProfileTarget{},
			DockerConfigKey:   "",
			DockerConfigItems: []DockerConfigItemInfo{},
		}

		// Process targets
		for _, to := range secretDef.To {
			profile.Targets = append(profile.Targets, ClusterProfileTarget{
				ClusterGroups: to.ClusterGroups,
				Namespace:     to.Namespace,
			})
		}

		// Process fields from the "from" section
		for k8sFieldName, fieldDef := range secretDef.From {
			if fieldDef.DockerConfigJSONData != nil {
				// Handle dockerconfigJSON - multiple items
				// Set the dockerconfig key name (e.g., "pull-secret")
				profile.DockerConfigKey = k8sFieldName

				for _, registry := range fieldDef.DockerConfigJSONData {
					item := registry.Item
					profile.Fields = append(profile.Fields, FieldMapping{
						Field:    k8sFieldName,
						FromItem: item,
						Pattern:  "dockerconfig",
					})

					// Store full registry info for bundle generation
					profile.DockerConfigItems = append(profile.DockerConfigItems, DockerConfigItemInfo{
						Item:        item,
						RegistryURL: registry.RegistryURL,
						AuthField:   registry.AuthField,
						EmailField:  registry.EmailField,
					})

					// Track item usage
					trackItemUsage(report.ItemUsageMap, item, profile.Name)
					categorizeItem(&profile.Items, item)
				}
			} else if fieldDef.Field != "" && fieldDef.Item != "" {
				// Simple field mapping
				item := fieldDef.Item
				profile.Fields = append(profile.Fields, FieldMapping{
					Field:    k8sFieldName,
					FromItem: item,
					Pattern:  "simple_field",
				})

				// Track item usage
				trackItemUsage(report.ItemUsageMap, item, profile.Name)
				categorizeItem(&profile.Items, item)
			}
		}

		report.Definitions = append(report.Definitions, profile)
		report.TotalDefinitions++
	}

	logrus.Infof("Found %d cluster profile definitions", report.TotalDefinitions)
	logrus.Infof("Unique items referenced: %d", len(report.ItemUsageMap))

	return report, nil
}

// trackItemUsage records which cluster profiles use an item
func trackItemUsage(usageMap map[string]ItemUsage, item string, profileName string) {
	usage := usageMap[item]
	usage.UsedByProfiles = append(usage.UsedByProfiles, profileName)
	usage.UsageCount++
	usageMap[item] = usage
}

// categorizeItem determines if an item is from kv/dptp or kv/selfservice
// Based on naming patterns - DPTP items are typically kebab-case secrets
// Selfservice items would have collection/group structure but in _config.yaml they're just item names
func categorizeItem(items *ClusterProfileItems, item string) {
	// In _config.yaml, all items are referenced by name only
	// They're typically kv/dptp/* secrets (openshift-ci-*-aws-credentials, pull secrets, etc.)
	// We'll categorize based on common DPTP naming patterns

	// Common DPTP item patterns
	isDPTP := false
	dptpPatterns := []string{
		"openshift-ci",
		"pull-secret",
		"quay",
		"registry",
		"build_farm",
		"mirror.openshift.com",
		"insights-ci",
	}

	for _, pattern := range dptpPatterns {
		if contains(item, pattern) {
			isDPTP = true
			break
		}
	}

	if isDPTP {
		items.DPTP = append(items.DPTP, item)
	} else {
		items.Selfservice = append(items.Selfservice, item)
	}
}

// contains checks if s contains substr (case-sensitive)
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			findInString(s, substr)))
}

func findInString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

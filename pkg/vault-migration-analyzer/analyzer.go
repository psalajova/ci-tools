package vaultmigrationanalyzer

import (
	"fmt"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"github.com/sirupsen/logrus"
)

// Note: AnalyzeClusterProfiles is implemented in cluster_profiles.go

// AnalyzeMultiStageCredentials analyzes multi-stage credential usage
func AnalyzeMultiStageCredentials(releaseRepoPath string) (*MultiStageCredReport, error) {
	logrus.Info("Multi-stage credential analysis not yet implemented")
	return &MultiStageCredReport{
		CredentialUsage: make(map[string]CredentialUsage),
	}, nil
}

// AnalyzeFieldMappingPatterns analyzes field mapping patterns from cluster profiles
func AnalyzeFieldMappingPatterns(clusterProfiles *ClusterProfileReport) *FieldMappingReport {
	report := &FieldMappingReport{
		Patterns: make(map[string]FieldMappingPattern),
	}

	patternCounts := make(map[string]int)
	patternExamples := make(map[string][]FieldMapping)

	// Count patterns
	for _, profile := range clusterProfiles.Definitions {
		for _, field := range profile.Fields {
			patternCounts[field.Pattern]++

			// Keep up to 3 examples per pattern
			if len(patternExamples[field.Pattern]) < 3 {
				patternExamples[field.Pattern] = append(patternExamples[field.Pattern], field)
			}
		}
	}

	// Build pattern report
	for pattern, count := range patternCounts {
		report.Patterns[pattern] = FieldMappingPattern{
			PatternType: pattern,
			Count:       count,
			Examples:    patternExamples[pattern],
		}
	}

	logrus.Infof("Field mapping patterns: simple_field=%d, dockerconfig=%d",
		patternCounts["simple_field"], patternCounts["dockerconfig"])

	return report
}

// AnalyzeTargetDistribution analyzes target distribution (cluster_groups/namespaces)
func AnalyzeTargetDistribution(clusterProfiles *ClusterProfileReport) *TargetDistributionReport {
	report := &TargetDistributionReport{
		UniqueClusterGroups: []string{},
		UniqueNamespaces:    []string{},
		CommonPatterns:      []TargetPattern{},
	}

	clusterGroupsSet := make(map[string]bool)
	namespacesSet := make(map[string]bool)
	patternCounts := make(map[string]*TargetPattern)

	// Collect unique values and patterns
	for _, profile := range clusterProfiles.Definitions {
		for _, target := range profile.Targets {
			// Collect namespaces
			if target.Namespace != "" {
				namespacesSet[target.Namespace] = true
			}

			// Collect cluster groups
			for _, cg := range target.ClusterGroups {
				clusterGroupsSet[cg] = true
			}

			// Build pattern key
			patternKey := fmt.Sprintf("%v|%s", target.ClusterGroups, target.Namespace)
			if pattern, exists := patternCounts[patternKey]; exists {
				pattern.SecretCount++
			} else {
				patternCounts[patternKey] = &TargetPattern{
					ClusterGroups: target.ClusterGroups,
					Namespace:     target.Namespace,
					SecretCount:   1,
				}
			}
		}
	}

	// Convert sets to slices
	for cg := range clusterGroupsSet {
		report.UniqueClusterGroups = append(report.UniqueClusterGroups, cg)
	}
	for ns := range namespacesSet {
		report.UniqueNamespaces = append(report.UniqueNamespaces, ns)
	}

	// Convert pattern map to slice
	for _, pattern := range patternCounts {
		report.CommonPatterns = append(report.CommonPatterns, *pattern)
	}

	logrus.Infof("Target distribution: %d unique cluster_groups, %d unique namespaces, %d patterns",
		len(report.UniqueClusterGroups), len(report.UniqueNamespaces), len(report.CommonPatterns))

	return report
}

// AnalyzeGSMQuota estimates GSM quota requirements
func AnalyzeGSMQuota(allSecrets []VaultSecret, gsmClient *secretmanager.Client, projectNumber string) *GSMQuotaReport {
	totalSecrets := 0
	for _, secret := range allSecrets {
		if !secret.IsEmpty && !secret.IsPlaceholder {
			totalSecrets += secret.FieldCount
		}
	}

	report := &GSMQuotaReport{
		TotalSecretsToCreate: totalSecrets,
		EstimatedVersions:    totalSecrets, // 1 version per secret initially
		MigrationFeasible:    true,         // Assume feasible for now
		QuotaIncreaseNeeded:  false,
		CurrentQuota: QuotaInfo{
			MaxSecrets:   100000, // Default GCP quota
			CurrentUsage: 0,      // Would need to query GSM
			Available:    100000,
		},
	}

	if gsmClient != nil {
		// Could query actual quota here, but for now just estimate
		logrus.Debugf("GSM client available, could query actual quota")
	}

	// Check if we'd exceed quota
	if report.TotalSecretsToCreate > report.CurrentQuota.Available {
		report.MigrationFeasible = false
		report.QuotaIncreaseNeeded = true
		report.RecommendedQuota = report.TotalSecretsToCreate + 10000 // Add buffer
	}

	return report
}

// AnalyzeCrossReference performs cross-reference and anomaly detection
func AnalyzeCrossReference(allSecrets []VaultSecret, clusterProfiles *ClusterProfileReport, multiStage *MultiStageCredReport) *CrossReferenceReport {
	logrus.Info("Cross-reference analysis not yet implemented")
	return &CrossReferenceReport{
		DualPurposeSecrets:           []DualPurposeSecret{},
		SecretSyncMetadataMismatches: []SecretSyncMismatch{},
		OrphanedSecrets:              []string{},
		MissingSecrets:               []MissingSecret{},
	}
}

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"github.com/sirupsen/logrus"
	"sigs.k8s.io/yaml"

	analyzer "github.com/openshift/ci-tools/pkg/vault-migration-analyzer"
	"github.com/openshift/ci-tools/pkg/vaultclient"
)

type options struct {
	releaseRepoPath  string
	gsmProjectNumber string
	outputFile       string
	blockersOnly     bool
	checkPhase       string
	logLevel         string
	generateBundles  bool
	bundlesOutput    string
	ocpContext       string
	ocpNamespace     string
}

func parseOptions() *options {
	o := &options{}
	flag.StringVar(&o.releaseRepoPath, "release-repo", os.Getenv("RELEASE_REPO"), "Path to local release repo")
	flag.StringVar(&o.gsmProjectNumber, "gsm-project-number", "384486694155", "GSM project number")
	flag.StringVar(&o.outputFile, "output", "migration-analysis.yaml", "Output file path")
	flag.BoolVar(&o.blockersOnly, "blockers-only", false, "Only check for blockers (fast mode)")
	flag.StringVar(&o.checkPhase, "check", "", "Check specific phase only (vault, cluster-profiles, field-mappings, targets, quota)")
	flag.StringVar(&o.logLevel, "log-level", "info", "Log level (panic, fatal, error, warn, info, debug, trace)")
	flag.BoolVar(&o.generateBundles, "generate-bundles", false, "Generate gsm-config.yaml bundles from cluster profiles")
	flag.StringVar(&o.bundlesOutput, "bundles-output", "cluster-profile-bundles.yaml", "Output file for generated bundles")
	flag.StringVar(&o.ocpContext, "ocp-context", "b01", "OCP context for verification queries")
	flag.StringVar(&o.ocpNamespace, "ocp-namespace", "ci", "OCP namespace for cluster profile secrets")
	flag.Parse()
	return o
}

func (o *options) Validate() error {
	if o.releaseRepoPath == "" {
		return fmt.Errorf("--release-repo is required")
	}
	return nil
}

func (o *options) setupLogger() {
	level, err := logrus.ParseLevel(o.logLevel)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to parse log level")
	}
	logrus.SetLevel(level)
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
		ForceColors:   true,
	})
}

func main() {
	o := parseOptions()
	if err := o.Validate(); err != nil {
		logrus.WithError(err).Fatal("Invalid options")
	}

	o.setupLogger()

	logrus.Info("=== Vault Migration Analyzer ===")
	logrus.Infof("Release repo: %s", o.releaseRepoPath)
	logrus.Infof("Output: %s", o.outputFile)

	// Setup Vault client
	vaultAddr := os.Getenv("VAULT_ADDR")
	if vaultAddr == "" {
		vaultAddr = "https://vault.ci.openshift.org"
	}
	vaultToken := os.Getenv("VAULT_TOKEN")
	if vaultToken == "" {
		logrus.Fatal("VAULT_TOKEN environment variable is required")
	}

	vaultClient, err := vaultclient.New(vaultAddr, vaultToken)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to create Vault client")
	}

	// Setup GSM client (for quota check)
	ctx := context.Background()
	gsmClient, err := secretmanager.NewClient(ctx)
	if err != nil {
		logrus.WithError(err).Warn("Failed to create GSM client, quota check will be skipped")
	} else {
		defer gsmClient.Close()
	}

	// Run analysis
	report, err := runAnalysis(o, vaultClient, gsmClient)
	if err != nil {
		logrus.WithError(err).Fatal("Analysis failed")
	}

	// Write report
	if err := writeReport(report, o.outputFile); err != nil {
		logrus.WithError(err).Fatal("Failed to write report")
	}

	logrus.Infof("Analysis complete! Report written to %s", o.outputFile)

	// Print summary
	printSummary(report)

	// Generate bundles if requested
	if o.generateBundles {
		if err := generateBundles(o, report, vaultClient); err != nil {
			logrus.WithError(err).Fatal("Bundle generation failed")
		}
	}
}

func runAnalysis(o *options, vaultClient *vaultclient.VaultClient, gsmClient *secretmanager.Client) (*analyzer.Report, error) {
	report := &analyzer.Report{}

	// Phase 0-1: Vault Inventory
	logrus.Info("")
	logrus.Info("=== Phase 0-1: Vault Inventory & Validation ===")
	vaultReport, allSecrets, err := analyzer.AnalyzeVaultInventory(vaultClient)
	if err != nil {
		return nil, fmt.Errorf("vault inventory failed: %w", err)
	}
	report.VaultInventory = *vaultReport

	if o.blockersOnly || o.checkPhase == "vault" {
		// Calculate readiness from vault data
		report.MigrationReadiness = calculateReadiness(report, allSecrets)
		return report, nil
	}

	// Phase 2: Cluster Profiles
	if o.checkPhase == "" || o.checkPhase == "cluster-profiles" {
		logrus.Info("")
		logrus.Info("=== Phase 2: Cluster Profile Analysis ===")
		clusterProfileReport, err := analyzer.AnalyzeClusterProfiles(o.releaseRepoPath)
		if err != nil {
			logrus.WithError(err).Warn("Cluster profile analysis failed, continuing...")
		} else {
			report.ClusterProfiles = *clusterProfileReport
		}
	}

	// Phase 3: Multi-Stage Credentials
	if o.checkPhase == "" {
		logrus.Info("")
		logrus.Info("=== Phase 3: Multi-Stage Credentials ===")
		multiStageReport, err := analyzer.AnalyzeMultiStageCredentials(o.releaseRepoPath)
		if err != nil {
			logrus.WithError(err).Warn("Multi-stage credentials analysis failed, continuing...")
		} else {
			report.MultiStageCredentials = *multiStageReport
		}
	}

	// Phase 4: Field Mapping Patterns
	if o.checkPhase == "" || o.checkPhase == "field-mappings" {
		logrus.Info("")
		logrus.Info("=== Phase 4: Field Mapping Patterns ===")
		if report.ClusterProfiles.TotalDefinitions > 0 {
			fieldMappingReport := analyzer.AnalyzeFieldMappingPatterns(&report.ClusterProfiles)
			report.FieldMappingPatterns = *fieldMappingReport
		}
	}

	// Phase 5: Target Distribution
	if o.checkPhase == "" || o.checkPhase == "targets" {
		logrus.Info("")
		logrus.Info("=== Phase 5: Target Distribution ===")
		if report.ClusterProfiles.TotalDefinitions > 0 {
			targetReport := analyzer.AnalyzeTargetDistribution(&report.ClusterProfiles)
			report.TargetDistribution = *targetReport
		}
	}

	// Phase 6: GSM Quota
	if o.checkPhase == "" || o.checkPhase == "quota" {
		logrus.Info("")
		logrus.Info("=== Phase 6: GSM Quota Estimation ===")
		quotaReport := analyzer.AnalyzeGSMQuota(allSecrets, gsmClient, o.gsmProjectNumber)
		report.GSMQuota = *quotaReport
	}

	// Phase 7: Cross-Reference
	if o.checkPhase == "" {
		logrus.Info("")
		logrus.Info("=== Phase 7: Cross-Reference & Anomalies ===")
		crossRefReport := analyzer.AnalyzeCrossReference(allSecrets, &report.ClusterProfiles, &report.MultiStageCredentials)
		report.CrossReference = *crossRefReport
	}

	// Calculate migration readiness
	report.MigrationReadiness = calculateReadiness(report, allSecrets)

	return report, nil
}

func calculateReadiness(report *analyzer.Report, allSecrets []analyzer.VaultSecret) analyzer.MigrationReadinessReport {
	readiness := analyzer.MigrationReadinessReport{
		CriticalBlockers: analyzer.BlockersReport{},
		FieldMappingCoverage: analyzer.CoverageReport{
			AllPatternsUnderstood: true,
		},
		DeduplicationIntelligence: analyzer.DeduplicationReport{
			SecretsUsedMultipleTimes: make(map[string]analyzer.MultiUseSecret),
		},
	}

	// Count total secrets to migrate (excluding empty/placeholder)
	for _, secret := range allSecrets {
		if !secret.IsEmpty && !secret.IsPlaceholder {
			readiness.TotalSecretsToMigrate++
			readiness.TotalGSMSecretsToCreate += secret.FieldCount
		}
	}

	// Count blockers
	readiness.CriticalBlockers.UnexpectedVaultPaths = len(report.VaultInventory.UnexpectedPaths)
	readiness.CriticalBlockers.ValidationFailures = len(report.VaultInventory.ValidationFailures.UnsupportedCharsInNames) +
		len(report.VaultInventory.ValidationFailures.UnsupportedCharsInFields)
	readiness.CriticalBlockers.NameLengthViolations = len(report.VaultInventory.ValidationFailures.NameLengthExceeded)
	readiness.CriticalBlockers.MissingSecrets = len(report.CrossReference.MissingSecrets)
	readiness.CriticalBlockers.QuotaInsufficient = !report.GSMQuota.MigrationFeasible

	// Check for exotic field mapping patterns
	for patternType, pattern := range report.FieldMappingPatterns.Patterns {
		if patternType == "exotic" && pattern.Count > 0 {
			readiness.FieldMappingCoverage.AllPatternsUnderstood = false
			for _, example := range pattern.Examples {
				readiness.FieldMappingCoverage.ExoticPatternsNeedingReview = append(
					readiness.FieldMappingCoverage.ExoticPatternsNeedingReview,
					example.Field,
				)
			}
		}
	}

	// Build deduplication intelligence from item usage
	for item, usage := range report.ClusterProfiles.ItemUsageMap {
		if usage.UsageCount > 1 {
			readiness.DeduplicationIntelligence.SecretsUsedMultipleTimes[item] = analyzer.MultiUseSecret{
				UsageCount: usage.UsageCount,
				UsedBy:     usage.UsedByProfiles,
			}
		}
	}

	return readiness
}

func writeReport(report *analyzer.Report, outputFile string) error {
	data, err := yaml.Marshal(report)
	if err != nil {
		return fmt.Errorf("failed to marshal report: %w", err)
	}

	if err := os.WriteFile(outputFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

func printSummary(report *analyzer.Report) {
	logrus.Info("")
	logrus.Info("=== SUMMARY ===")
	logrus.Infof("Vault Secrets: %d DPTP + %d Selfservice = %d total",
		report.VaultInventory.TotalDPTPSecrets,
		report.VaultInventory.TotalSelfserviceSecrets,
		report.VaultInventory.TotalDPTPSecrets+report.VaultInventory.TotalSelfserviceSecrets)
	logrus.Infof("Secrets to migrate: %d", report.MigrationReadiness.TotalSecretsToMigrate)
	logrus.Infof("GSM secrets to create: %d", report.MigrationReadiness.TotalGSMSecretsToCreate)

	logrus.Info("")
	logrus.Info("Critical Blockers:")
	if report.MigrationReadiness.CriticalBlockers.UnexpectedVaultPaths > 0 {
		logrus.Warnf("  ⚠ Unexpected Vault paths: %d", report.MigrationReadiness.CriticalBlockers.UnexpectedVaultPaths)
	}
	if report.MigrationReadiness.CriticalBlockers.ValidationFailures > 0 {
		logrus.Warnf("  ⚠ Validation failures: %d", report.MigrationReadiness.CriticalBlockers.ValidationFailures)
	}
	if report.MigrationReadiness.CriticalBlockers.NameLengthViolations > 0 {
		logrus.Warnf("  ⚠ Name length violations: %d", report.MigrationReadiness.CriticalBlockers.NameLengthViolations)
	}
	if report.MigrationReadiness.CriticalBlockers.MissingSecrets > 0 {
		logrus.Warnf("  ⚠ Missing secrets: %d", report.MigrationReadiness.CriticalBlockers.MissingSecrets)
	}
	if report.MigrationReadiness.CriticalBlockers.QuotaInsufficient {
		logrus.Warnf("  ⚠ Insufficient GSM quota")
	}

	if report.MigrationReadiness.CriticalBlockers.UnexpectedVaultPaths == 0 &&
		report.MigrationReadiness.CriticalBlockers.ValidationFailures == 0 &&
		report.MigrationReadiness.CriticalBlockers.NameLengthViolations == 0 &&
		report.MigrationReadiness.CriticalBlockers.MissingSecrets == 0 &&
		!report.MigrationReadiness.CriticalBlockers.QuotaInsufficient {
		logrus.Info("  ✅ No critical blockers found!")
	}

	logrus.Info("")
	logrus.Infof("Normalization: %d dots, %d underscores converted",
		report.VaultInventory.NormalizationApplied.DotsConverted,
		report.VaultInventory.NormalizationApplied.UnderscoresConverted)

	if len(report.VaultInventory.EmptyOrPlaceholder) > 0 {
		logrus.Infof("Empty/placeholder secrets: %d", len(report.VaultInventory.EmptyOrPlaceholder))
	}

	logrus.Info("")
	logrus.Info("Full report written to:", logrus.Fields{"file": report})
}

func generateBundles(o *options, report *analyzer.Report, vaultClient *vaultclient.VaultClient) error {
	logrus.Info("")
	logrus.Info("=== Generating Cluster Profile Bundles ===")

	// Re-scan Vault to get full secret data with metadata
	vaultReport, allSecrets, err := analyzer.AnalyzeVaultInventory(vaultClient)
	if err != nil {
		return fmt.Errorf("failed to scan Vault: %w", err)
	}
	logrus.Infof("Loaded %d Vault secrets", len(allSecrets))
	_ = vaultReport // unused but needed for the call

	// Create OCP querier
	ocpQuerier := analyzer.NewOCCommandQuerier(o.ocpContext, o.ocpNamespace)

	// Generate bundles
	result, err := analyzer.GenerateBundles(
		&report.ClusterProfiles,
		allSecrets,
		ocpQuerier,
		o.ocpContext,
		o.ocpNamespace,
	)
	if err != nil {
		return fmt.Errorf("bundle generation failed: %w", err)
	}

	// Print results
	logrus.Infof("Generated %d bundles", len(result.Bundles))
	if len(result.Warnings) > 0 {
		logrus.Warnf("Warnings: %d", len(result.Warnings))
		for _, warn := range result.Warnings {
			logrus.Warn("  " + warn)
		}
	}
	if len(result.Errors) > 0 {
		logrus.Errorf("Errors: %d", len(result.Errors))
		for _, errMsg := range result.Errors {
			logrus.Error("  " + errMsg)
		}
	}
	if len(result.SkippedProfiles) > 0 {
		logrus.Warnf("Skipped profiles: %d", len(result.SkippedProfiles))
	}

	// Export bundles to YAML
	bundlesYAML, err := analyzer.ExportBundles(result.Bundles)
	if err != nil {
		return fmt.Errorf("failed to export bundles: %w", err)
	}

	// Write to file
	if err := os.WriteFile(o.bundlesOutput, bundlesYAML, 0644); err != nil {
		return fmt.Errorf("failed to write bundles file: %w", err)
	}

	logrus.Infof("Bundles written to %s", o.bundlesOutput)
	return nil
}

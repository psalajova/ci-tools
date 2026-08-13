package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"github.com/sirupsen/logrus"

	gsmassmigration "github.com/openshift/ci-tools/pkg/gsm-mass-migration"
	"github.com/openshift/ci-tools/pkg/vaultclient"
)

// setupVaultClientForMigration creates a vaultclient.VaultClient for the migration.
// If VAULT_TOKEN is missing or expired, triggers interactive OIDC login.
func setupVaultClientForMigration() (*vaultclient.VaultClient, error) {
	vaultAddr := os.Getenv("VAULT_ADDR")
	if vaultAddr == "" {
		vaultAddr = "https://vault.ci.openshift.org"
	}

	vaultToken := os.Getenv("VAULT_TOKEN")
	if vaultToken == "" {
		logrus.Info("VAULT_TOKEN not set, attempting OIDC login...")
		var err error
		vaultToken, err = vaultOIDCLogin(vaultAddr)
		if err != nil {
			return nil, fmt.Errorf("vault OIDC login failed: %w", err)
		}
	}

	logrus.Debugf("Vault client setup: addr=%s, token_length=%d", vaultAddr, len(vaultToken))

	vaultClient, err := vaultclient.New(vaultAddr, vaultToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create Vault client: %w", err)
	}

	// Test the client by listing kv/selfservice
	testPaths, err := vaultClient.ListKV("kv/selfservice")
	if err != nil {
		logrus.Warn("Vault token appears expired, attempting OIDC login...")
		vaultToken, err = vaultOIDCLogin(vaultAddr)
		if err != nil {
			return nil, fmt.Errorf("vault OIDC login failed: %w", err)
		}
		vaultClient, err = vaultclient.New(vaultAddr, vaultToken)
		if err != nil {
			return nil, fmt.Errorf("failed to create Vault client after re-login: %w", err)
		}
		testPaths, err = vaultClient.ListKV("kv/selfservice")
		if err != nil {
			return nil, fmt.Errorf("vault still not accessible after re-login: %w", err)
		}
	}
	logrus.Debugf("Vault client test: found %d top-level keys in kv/selfservice", len(testPaths))

	return vaultClient, nil
}

// vaultOIDCLogin runs 'vault login -method=oidc' and returns the token
func vaultOIDCLogin(vaultAddr string) (string, error) {
	cmd := exec.Command("vault", "login", "-method=oidc", "-token-only")
	cmd.Env = append(os.Environ(), "VAULT_ADDR="+vaultAddr)
	cmd.Stderr = os.Stderr
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("'vault login -method=oidc' failed: %w", err)
	}
	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", fmt.Errorf("vault login returned empty token")
	}
	logrus.Info("Vault OIDC login successful")
	return token, nil
}

// loadVaultCache loads the Vault cache from disk or builds it from Vault
func loadVaultCache(o *options) (*gsmassmigration.VaultCache, error) {
	if o.vaultCacheFile != "" {
		cache, err := gsmassmigration.LoadFromFile(o.vaultCacheFile)
		if err == nil {
			return cache, nil
		}
		logrus.WithError(err).Info("No cache file found, building from Vault...")
	}

	vaultClient, err := setupVaultClientForMigration()
	if err != nil {
		return nil, err
	}

	cache, err := gsmassmigration.BuildVaultCache(vaultClient)
	if err != nil {
		return nil, fmt.Errorf("failed to build Vault cache: %w", err)
	}

	if o.vaultCacheFile != "" {
		if err := cache.SaveToFile(o.vaultCacheFile); err != nil {
			logrus.WithError(err).Warn("Failed to save Vault cache to disk")
		}
	}

	return cache, nil
}

// runMassMigration executes the mass migration workflow
func (o *options) runMassMigration() error {
	logrus.Info("Starting mass migration from Vault to GSM")

	cache, err := loadVaultCache(o)
	if err != nil {
		return err
	}

	ctx := context.Background()

	// Track migrations from both paths
	var allMigrations []gsmassmigration.MigrationResult
	var credUpdates []gsmassmigration.CredentialUpdate
	var configUpdateResult *gsmassmigration.ConfigUpdateResult
	var indexUpdateFailures []string

	// Setup GSM client (only if we're actually creating secrets)
	var gsmClient *secretmanager.Client
	if !o.skipGSMCreation {
		var gsmErr error
		gsmClient, gsmErr = secretmanager.NewClient(ctx)
		if gsmErr != nil {
			return fmt.Errorf("failed to create GSM client: %w", gsmErr)
		}
		defer gsmClient.Close()
	} else {
		logrus.Info("GSM secret creation skipped (--skip-gsm-creation), using Vault cache for credential mapping")
	}

	// === Credential Path ===
	if o.migrateAll || o.migrateCredentialsOnly {
		logrus.Info("=== Credential Migration Path ===")

		// Phase 1: Find credentials to migrate
		logrus.Info("Phase 1: Finding credentials to migrate...")
		var selfserviceSecretsToMigrate []gsmassmigration.VaultSecretPath

		if o.targetedMode {
			selfserviceSecretsToMigrate, err = o.enumerateTargetedSecrets(cache)
			if err != nil {
				return fmt.Errorf("failed to enumerate targeted secrets: %w", err)
			}
			logrus.Infof("Found %d secrets used by %s/%s", len(selfserviceSecretsToMigrate), o.org, o.repo)
		} else {
			selfserviceSecretsToMigrate = o.enumerateSelfserviceSecretsFromCache(cache)
			logrus.Infof("Found %d unique selfservice secrets in cache", len(selfserviceSecretsToMigrate))
		}

		// Phase 3a: Migrate selfservice secrets to GSM (or synthesize results from cache)
		if len(selfserviceSecretsToMigrate) > 0 {
			if o.skipGSMCreation {
				logrus.Info("Phase 3a: Synthesizing migration results from Vault cache...")
				allMigrations = append(allMigrations, synthesizeMigrationResults(cache, selfserviceSecretsToMigrate)...)
			} else {
				logrus.Info("Phase 3a: Migrating selfservice secrets to GSM...")
				migrations, err := gsmassmigration.MigrateSecrets(
					ctx,
					cache,
					gsmClient,
					selfserviceSecretsToMigrate,
					o.gsmProjectNumber,
					o.dryRun,
					false,
				)
				if err != nil {
					return fmt.Errorf("selfservice migration failed: %w", err)
				}
				allMigrations = append(allMigrations, migrations...)

				failedCollections, err := gsmassmigration.UpdateCollectionIndexes(ctx, gsmClient, o.gsmProjectNumber, migrations, o.dryRun)
				if err != nil {
					return fmt.Errorf("failed to update collection indexes after selfservice migration: %w", err)
				}
				indexUpdateFailures = append(indexUpdateFailures, failedCollections...)
			}
		} else {
			logrus.Warn("No selfservice secrets found to migrate")
		}

		// Phase 3c: Generate bundles for multi-source credentials (multiple vault paths -> one K8s secret)
		logrus.Info("Phase 3c: Generating bundles for multi-source credentials...")
		multiSourceAdded, err := gsmassmigration.GenerateMultiSourceBundles(cache, o.releaseRepoPath, o.dryRun)
		if err != nil {
			return fmt.Errorf("failed to generate multi-source bundles: %w", err)
		}
		if multiSourceAdded > 0 {
			logrus.Infof("Added %d multi-source credential bundles to gsm-config.yaml", multiSourceAdded)
		}

		// Phase 4: Update credential stanzas
		logrus.Info("Phase 4: Updating credential stanzas in configs and step-registry...")
		credUpdates, err = gsmassmigration.UpdateCredentialStanzas(
			o.releaseRepoPath,
			allMigrations,
			o.skipCSIFlag,
			o.dryRun,
		)
		if err != nil {
			return fmt.Errorf("failed to update credential stanzas: %w", err)
		}
	}

	// === Cluster Profile Path ===
	if o.migrateAll || o.migrateClusterProfilesOnly {
		logrus.Info("=== Cluster Profile Migration Path ===")

		// Phase 2a: Extract cluster profiles from _config.yaml
		logrus.Info("Phase 2a: Extracting cluster profiles from _config.yaml...")
		clusterProfileSecrets, _, dptpItemsFromConfig, err := gsmassmigration.ExtractClusterProfiles(o.releaseRepoPath)
		if err != nil {
			return fmt.Errorf("failed to extract cluster profiles: %w", err)
		}
		logrus.Infof("Found %d cluster profile definitions, %d unique DPTP items", len(clusterProfileSecrets), len(dptpItemsFromConfig))

		// Phase 2a-2: Discover user-secret-only profiles (not in _config.yaml)
		logrus.Info("Phase 2a-2: Discovering user-secret-only cluster profiles from Vault cache...")
		userSecretOnlyProfiles, err := gsmassmigration.DiscoverUserSecretOnlyProfiles(o.releaseRepoPath, cache, clusterProfileSecrets)
		if err != nil {
			return fmt.Errorf("failed to discover user-secret-only profiles: %w", err)
		}
		clusterProfileSecrets = append(clusterProfileSecrets, userSecretOnlyProfiles...)

		// Phase 3b: Migrate DPTP secrets to GSM
		if !o.skipGSMCreation {
			logrus.Info("Phase 3b: Migrating DPTP secrets to GSM...")
			dptpSecretsFromCache := cache.FilterDPTPByItems(dptpItemsFromConfig)
			logrus.Infof("Found %d DPTP secrets to migrate", len(dptpSecretsFromCache))

			if len(dptpSecretsFromCache) > 0 {
				var dptpSecretsToMigrate []gsmassmigration.VaultSecretPath
				for _, cached := range dptpSecretsFromCache {
					dptpSecretsToMigrate = append(dptpSecretsToMigrate, gsmassmigration.VaultSecretPath{
						FullPath:   cached.Path,
						Collection: cached.Collection,
						Group:      cached.Group,
					})
				}

				migrations, err := gsmassmigration.MigrateSecrets(
					ctx,
					cache,
					gsmClient,
					dptpSecretsToMigrate,
					o.gsmProjectNumber,
					o.dryRun,
					true,
				)
				if err != nil {
					return fmt.Errorf("DPTP migration failed: %w", err)
				}
				allMigrations = append(allMigrations, migrations...)

				failedCollections, err := gsmassmigration.UpdateCollectionIndexes(ctx, gsmClient, o.gsmProjectNumber, migrations, o.dryRun)
				if err != nil {
					return fmt.Errorf("failed to update collection indexes after DPTP migration: %w", err)
				}
				indexUpdateFailures = append(indexUpdateFailures, failedCollections...)
			}
		} else {
			logrus.Info("Phase 3b: Skipped DPTP secret migration (--skip-gsm-creation)")
		}

		// Phase 2b: Generate bundles and update configs
		logrus.Info("Phase 2b: Generating bundles for cluster profiles...")
		configResult, err := o.generateAndUpdateBundlesFromCache(clusterProfileSecrets, cache)
		if err != nil {
			return fmt.Errorf("failed to generate bundles: %w", err)
		}
		configUpdateResult = configResult
	}

	// Phase 5: Generate report (ALWAYS runs)
	logrus.Info("Phase 5: Generating migration report...")
	report := gsmassmigration.GenerateReport(allMigrations, credUpdates, cache)
	if configUpdateResult != nil {
		report.BundlesAddedToGSMConfig = configUpdateResult.BundlesAdded
		report.ConfigEntriesRemovedFromConfig = configUpdateResult.ConfigEntriesRemoved
	}
	report.IndexUpdateFailures = indexUpdateFailures
	gsmassmigration.PrintReport(report)

	if report.FailedSecrets > 0 || len(report.IndexUpdateFailures) > 0 {
		return fmt.Errorf("migration completed with %d secret failures and %d collections needing an index re-run", report.FailedSecrets, len(report.IndexUpdateFailures))
	}

	logrus.Infof("Remember to run 'make update' in the release repo (%s) before committing", o.releaseRepoPath)

	return nil
}

// synthesizeMigrationResults builds MigrationResult entries from the Vault cache
// without actually creating GSM secrets. Used with --skip-gsm-creation.
func synthesizeMigrationResults(cache *gsmassmigration.VaultCache, secrets []gsmassmigration.VaultSecretPath) []gsmassmigration.MigrationResult {
	var results []gsmassmigration.MigrationResult
	for _, secret := range secrets {
		cached := cache.GetByPath(secret.FullPath)
		secretName := secret.Group
		if cached != nil && cached.TargetName != "" {
			secretName = cached.TargetName
		}
		targetNamespaces := ""
		if cached != nil {
			targetNamespaces = cached.TargetNamespace
		}
		results = append(results, gsmassmigration.MigrationResult{
			VaultPath:        secret.FullPath,
			Collection:       secret.Collection,
			Group:            secret.Group,
			SecretName:       secretName,
			TargetNamespaces: targetNamespaces,
		})
	}
	logrus.Infof("Synthesized %d migration results from cache", len(results))
	return results
}

// enumerateSelfserviceSecretsFromCache filters the cache for non-empty selfservice secrets
func (o *options) enumerateSelfserviceSecretsFromCache(cache *gsmassmigration.VaultCache) []gsmassmigration.VaultSecretPath {
	var secrets []gsmassmigration.VaultSecretPath

	// Get all secrets from cache
	for _, cached := range cache.Secrets {
		// Skip DPTP secrets (they're handled separately in cluster profile path)
		if cached.Collection == "test-platform-infra" {
			continue
		}

		// Skip empty/placeholder secrets
		if cached.IsEmpty || cached.IsPlaceholder {
			continue
		}

		secrets = append(secrets, gsmassmigration.VaultSecretPath{
			FullPath:   cached.Path,
			Collection: cached.Collection,
			Group:      cached.Group,
		})
	}

	return secrets
}

// enumerateTargetedSecrets finds secrets used by a specific org/repo by querying configs and OCP
func (o *options) enumerateTargetedSecrets(cache *gsmassmigration.VaultCache) ([]gsmassmigration.VaultSecretPath, error) {
	// Collect credential names and namespaces from configs/step-registry
	credentials := o.collectCredentialsFromOrgRepo()
	logrus.Infof("Found %d unique old-stanza credentials referenced by %s/%s", len(credentials), o.org, o.repo)

	var secretPaths []gsmassmigration.VaultSecretPath

	for _, cred := range credentials {
		// Query OCP to get the vault source path using the actual namespace from the config
		vaultPath, err := gsmassmigration.GetVaultPathFromOCPSecret(cred.name, cred.namespace)
		if err != nil {
			logrus.WithError(err).Warnf("Could not find vault path for credential %s/%s, skipping", cred.namespace, cred.name)
			continue
		}

		// Parse the vault path to extract collection/group
		collection, group, err := gsmassmigration.ParseVaultPath(vaultPath)
		if err != nil {
			logrus.WithError(err).Warnf("Invalid vault path for %s: %s, skipping", cred.name, vaultPath)
			continue
		}

		secretPaths = append(secretPaths, gsmassmigration.VaultSecretPath{
			FullPath:   vaultPath,
			Collection: collection,
			Group:      group,
		})

		logrus.Debugf("Credential %s/%s → vault path %s (collection=%s, group=%s)",
			cred.namespace, cred.name, vaultPath, collection, group)
	}

	return secretPaths, nil
}

// generateAndUpdateBundlesFromCache generates bundles using cached Vault data
func (o *options) generateAndUpdateBundlesFromCache(clusterProfiles []gsmassmigration.ClusterProfileSecret, cache *gsmassmigration.VaultCache) (*gsmassmigration.ConfigUpdateResult, error) {
	// Convert cache to VaultSecretData format for bundle generator
	var allVaultSecrets []gsmassmigration.VaultSecretData
	for _, cached := range cache.Secrets {
		allVaultSecrets = append(allVaultSecrets, gsmassmigration.VaultSecretData{
			Path:            cached.Path,
			Collection:      cached.Collection,
			Group:           cached.Group,
			TargetName:      cached.TargetName,
			TargetNamespace: cached.TargetNamespace,
			IsEmpty:         cached.IsEmpty,
			IsPlaceholder:   cached.IsPlaceholder,
		})
	}

	// Generate bundles using existing function
	return o.generateAndUpdateBundles(clusterProfiles, allVaultSecrets)
}

// generateAndUpdateBundles generates bundles and updates both config files
func (o *options) generateAndUpdateBundles(clusterProfiles []gsmassmigration.ClusterProfileSecret, allVaultSecrets []gsmassmigration.VaultSecretData) (*gsmassmigration.ConfigUpdateResult, error) {
	// Generate bundles
	result, err := gsmassmigration.GenerateBundles(
		clusterProfiles,
		allVaultSecrets,
	)
	if err != nil {
		return nil, fmt.Errorf("bundle generation failed: %w", err)
	}

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
		return nil, fmt.Errorf("bundle generation had %d errors", len(result.Errors))
	}
	if len(result.SkippedProfiles) > 0 {
		logrus.Warnf("Skipped profiles: %d", len(result.SkippedProfiles))
	}

	// Collect migrated profile names
	var migratedProfiles []string
	for _, bundle := range result.Bundles {
		migratedProfiles = append(migratedProfiles, bundle.Name)
	}

	// Update config files
	logrus.Info("Updating _config.yaml and gsm-config.yaml...")
	configResult, err := gsmassmigration.UpdateConfigFiles(
		o.releaseRepoPath,
		migratedProfiles,
		result.Bundles,
		o.dryRun,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to update config files: %w", err)
	}

	return configResult, nil
}

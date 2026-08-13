package gsmassmigration

import (
	"context"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/openshift/ci-tools/pkg/api"
	gsmsecrets "github.com/openshift/ci-tools/pkg/gsm-secrets"
	gsmvalidation "github.com/openshift/ci-tools/pkg/gsm-validation"
	vaultgsmmigration "github.com/openshift/ci-tools/pkg/vault-gsm-migration"
)

// secretsAlreadyInGSM lists DPTP items (groups in test-platform-infra collection) that are
// already managed in GSM by ci-secret-generator. These are periodically regenerated, so we
// must not overwrite them with stale Vault data during migration.
var secretsAlreadyInGSM = sets.New(
	"build_farm",
	"release-controller",
	"ci-chat-bot",
	"pod-scaler",
	"ship-status-dash-component-monitor",
	"openshift-monitoring-credentials",
	"ci-monitoring-thanos-tls",
	"quay-io-pull-credentials",
)

// MigrateSecrets migrates a list of Vault secrets to GSM using cached Vault data.
// For each secret:
//  1. Gets cached data from VaultCache (no Vault API calls)
//  2. Calls MigrateVaultSecretToGSMFromCache with cached fields
//  3. Collects results and continues on errors
//
// DPTP secrets are identified by collection="test-platform-infra" and get the dptp label.
// Secrets already managed in GSM by ci-secret-generator are skipped.
// Returns a list of MigrationResult with success/failure status for each secret.
func MigrateSecrets(
	ctx context.Context,
	cache *VaultCache,
	gsmClient gsmsecrets.SecretManagerClient,
	secrets []VaultSecretPath,
	projectNumber string,
	dryRun bool,
	_ bool, // Deprecated: isDPTP is now determined from secret.Collection
) ([]MigrationResult, error) {
	var results []MigrationResult

	for _, secret := range secrets {
		// Skip secrets already managed in GSM by ci-secret-generator
		if secret.Collection == api.DPTPGSMCollection && secretsAlreadyInGSM.Has(secret.Group) {
			logrus.Debugf("Skipping %s (already managed in GSM by ci-secret-generator)", secret.FullPath)
			continue
		}

		// Determine if this is a DPTP secret based on collection
		isDPTP := secret.Collection == api.DPTPGSMCollection

		result := MigrateSingleSecret(ctx, cache, gsmClient, secret, projectNumber, dryRun, isDPTP)
		results = append(results, result)

		if result.Error != nil {
			logrus.WithError(result.Error).Errorf("Failed to migrate %s", secret.FullPath)
		} else {
			logrus.Debugf("Migrated %s -> collection=%s, group=%s (%d fields)",
				secret.FullPath, result.Collection, result.Group, len(result.CreatedFields))
		}
	}

	return results, nil
}

// MigrateSingleSecret migrates a single Vault secret to GSM using cached data.
// Gets cached fields from VaultCache and calls MigrateVaultSecretToGSMFromCache.
// Returns a MigrationResult (never returns error - errors are captured in result.Error).
func MigrateSingleSecret(
	ctx context.Context,
	cache *VaultCache,
	gsmClient gsmsecrets.SecretManagerClient,
	vaultSecret VaultSecretPath,
	projectNumber string,
	dryRun bool,
	isDPTP bool,
) MigrationResult {
	result := MigrationResult{
		VaultPath:  vaultSecret.FullPath,
		Collection: vaultSecret.Collection,
		Group:      vaultSecret.Group,
	}

	// Get cached secret data
	cached := cache.GetByPath(vaultSecret.FullPath)
	if cached == nil {
		result.Error = fmt.Errorf("secret not found in cache: %s", vaultSecret.FullPath)
		return result
	}

	// For DPTP secrets, we don't check secretsync/target-name (they don't have it)
	// For user secrets, use cached TargetName
	var secretName string
	if isDPTP {
		// DPTP secrets don't have secretsync metadata - use the item name for reporting
		secretName = vaultSecret.Group
		logrus.Debugf("DPTP secret %s (item: %s)", vaultSecret.FullPath, secretName)
	} else {
		secretName = cached.TargetName
		if secretName == "" {
			logrus.Debugf("Secret %s has no secretsync/target-name, will migrate but cannot update credential stanzas", vaultSecret.FullPath)
			secretName = vaultSecret.Group
		}
	}
	result.SecretName = secretName
	result.TargetNamespaces = cached.TargetNamespace

	// Call the migration function with cached fields
	createdFields, err := vaultgsmmigration.MigrateVaultSecretToGSMFromCache(
		ctx,
		gsmClient,
		vaultSecret.Collection,
		vaultSecret.Group,
		cached.Fields, // Use cached fields instead of reading from Vault
		projectNumber,
		dryRun,
		isDPTP,
	)
	if err != nil {
		result.Error = fmt.Errorf("migration failed: %w", err)
		return result
	}

	result.CreatedFields = createdFields
	return result
}

// buildIndexUpdates groups migration results by collection, strips the collection
// prefix from each created field name, and merges with existing index entries.
// Returns a map of collection -> new index payload (ready to write to GSM).
func buildIndexUpdates(migrations []MigrationResult, existingByCollection map[string][]string) map[string][]byte {
	// Group created field names by collection
	byCollection := make(map[string][]string)
	for _, m := range migrations {
		if m.Error != nil || len(m.CreatedFields) == 0 {
			continue
		}
		prefix := m.Collection + gsmvalidation.CollectionSecretDelimiter
		for _, fullName := range m.CreatedFields {
			entry := strings.TrimPrefix(fullName, prefix)
			byCollection[m.Collection] = append(byCollection[m.Collection], entry)
		}
	}

	result := make(map[string][]byte)
	for collection, newEntries := range byCollection {
		existing := existingByCollection[collection]
		allEntries := sets.New(existing...)
		added := 0
		for _, entry := range newEntries {
			if !allEntries.Has(entry) {
				allEntries.Insert(entry)
				added++
			}
		}
		if added == 0 {
			continue
		}
		result[collection] = gsmsecrets.ConstructIndexSecretContent(allEntries.UnsortedList())
	}
	return result
}

// UpdateCollectionIndexes updates the index secret for each collection that had
// secrets created during migration. Groups created secrets by collection, reads
// the current index, merges new entries, and writes back.
//
// A collection is skipped (its index is left untouched) if its current index
// can't be read, or written back after merging. Skipped collections are
// returned so the caller can report them and re-run the migration for just
// those collections later -- migration and index updates are idempotent, so
// re-running is safe once the underlying issue is fixed.
func UpdateCollectionIndexes(
	ctx context.Context,
	gsmClient gsmsecrets.SecretManagerClient,
	projectNumber string,
	migrations []MigrationResult,
	dryRun bool,
) (skippedCollections []string, err error) {
	// Read existing indexes from GSM
	existingByCollection := make(map[string][]string)
	collections := sets.New[string]()
	for _, m := range migrations {
		if m.Error == nil && len(m.CreatedFields) > 0 {
			collections.Insert(m.Collection)
		}
	}

	readable := sets.New[string]()
	skipped := sets.New[string]()
	for collection := range collections {
		indexSecretName := gsmsecrets.GetIndexSecretName(collection)
		resourceName := fmt.Sprintf("%s/secrets/%s",
			gsmsecrets.GetProjectResourceIdNumber(projectNumber),
			indexSecretName,
		)
		payload, readErr := gsmsecrets.GetSecretPayload(ctx, gsmClient, resourceName)
		if readErr != nil {
			//todo uncomment
			//logrus.WithError(readErr).Errorf("Could not read index for collection %s -- skipping index update for it (secrets were still created; re-run the migration to update its index once this is fixed)", collection)
			skipped.Insert(collection)
			continue
		}
		existingByCollection[collection] = gsmsecrets.ParseIndexSecretContent(payload)
		readable.Insert(collection)
	}

	// Only merge/write updates for collections whose current index we could
	// actually read -- otherwise we'd overwrite the index based on an empty
	// read and lose existing entries.
	readableMigrations := make([]MigrationResult, 0, len(migrations))
	for _, m := range migrations {
		if readable.Has(m.Collection) {
			readableMigrations = append(readableMigrations, m)
		}
	}

	updates := buildIndexUpdates(readableMigrations, existingByCollection)

	for collection, payload := range updates {
		indexSecretName := gsmsecrets.GetIndexSecretName(collection)

		if dryRun {
			logrus.Infof("[DRY-RUN] Would update index for collection %s", collection)
			continue
		}

		annotations := map[string]string{
			"request-information": "Updated during Vault to GSM migration",
		}
		if writeErr := gsmsecrets.CreateOrUpdateSecret(ctx, gsmClient, projectNumber, indexSecretName, payload, nil, annotations); writeErr != nil {
			//todo uncomment
			//logrus.WithError(writeErr).Errorf("Failed to update index for collection %s -- re-run the migration to retry", collection)
			skipped.Insert(collection)
			continue
		}
		logrus.Infof("Updated index for collection %s", collection)
	}

	return sets.List(skipped), nil
}

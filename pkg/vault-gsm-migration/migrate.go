package vaultgsmmigration

import (
	"context"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	gsmsecrets "github.com/openshift/ci-tools/pkg/gsm-secrets"
	gsmvalidation "github.com/openshift/ci-tools/pkg/gsm-validation"
	"github.com/openshift/ci-tools/pkg/vaultclient"
)

// ExtractGroupFromVaultPath extracts the group name from a full Vault path.
// The path format is expected to be: mount/prefix/group
// Example: "kv/dptp/cloud.openshift.com-pull-secret" -> "cloud.openshift.com-pull-secret"
// Example: "kv/dptp/project/secret" -> "project/secret" (nested paths preserve slashes)
func ExtractGroupFromVaultPath(vaultPath string) (string, error) {
	parts := strings.SplitN(vaultPath, "/", 3) // Split into at most 3 parts
	if len(parts) < 3 {
		return "", fmt.Errorf("invalid vault path format, expected mount/prefix/group: %s", vaultPath)
	}
	// parts[0] = mount point (e.g., "kv")
	// parts[1] = prefix/namespace (e.g., "dptp")
	// parts[2] = group name (everything after second slash)
	return parts[2], nil
}

// GetFieldsFromVault reads all fields from a Vault secret, excluding secretsync/* metadata fields.
// Returns a map of field name -> field value.
func GetFieldsFromVault(vaultClient *vaultclient.VaultClient, vaultPath string) (map[string]string, error) {
	kvData, err := vaultClient.GetKV(vaultPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read secret from Vault at path %q: %w", vaultPath, err)
	}

	if kvData.Data == nil {
		return nil, fmt.Errorf("no data found in Vault secret at path %q", vaultPath)
	}

	// Filter out secretsync/* metadata fields
	fields := make(map[string]string)
	for key, value := range kvData.Data {
		if !strings.HasPrefix(key, "secretsync/") {
			fields[key] = value
		}
	}

	if len(fields) == 0 {
		return nil, fmt.Errorf("no non-metadata fields found in Vault secret at path %q", vaultPath)
	}

	return fields, nil
}

// MigrateVaultSecretToGSM reads a secret from Vault and creates GSM secrets for each field it contains in Vault.
// It normalizes the group name and field names to be compatible with GSM naming requirements.
// Returns a list of created GSM secret names and any error encountered.
//
// Parameters:
//   - ctx: Context for the operation
//   - vaultClient: Vault client for reading secrets
//   - gsmClient: GSM client for creating secrets
//   - vaultPath: Full Vault path (e.g., "kv/dptp/cloud.openshift.com-pull-secret")
//   - collection: GSM collection name
//   - group: GSM group name (if empty, extracted from vaultPath)
//   - projectNumber: GCP project number
//   - dryRun: If true, only log what would be created without actually creating secrets
//   - dptp: If true, adds label "jira-project":"dptp" to created secrets
func MigrateVaultSecretToGSM(
	ctx context.Context,
	vaultClient *vaultclient.VaultClient,
	gsmClient gsmsecrets.SecretManagerClient,
	vaultPath string,
	collection string,
	group string,
	projectNumber string,
	dryRun bool,
	dptp bool,
) ([]string, error) {
	// Extract group name from Vault path if not provided
	if group == "" {
		var err error
		group, err = ExtractGroupFromVaultPath(vaultPath)
		if err != nil {
			return nil, err
		}
		logrus.WithFields(logrus.Fields{"group": group}).Debug("extracted group from Vault path")
	}

	// Read fields from Vault
	fields, err := GetFieldsFromVault(vaultClient, vaultPath)
	if err != nil {
		return nil, err
	}

	logrus.Debugf("Found %d fields in Vault secret at %q", len(fields), vaultPath)
	for field, value := range fields {
		logrus.WithFields(logrus.Fields{"field": field, "value": value}).Debug("found field")
	}

	return MigrateVaultSecretToGSMFromCache(ctx, gsmClient, collection, group, fields, projectNumber, dryRun, dptp)
}

// MigrateVaultSecretToGSMFromCache creates GSM secrets from cached Vault fields (no Vault API calls).
// This is a cache-optimized version of MigrateVaultSecretToGSM that accepts pre-fetched fields.
// It normalizes the group name and field names to be compatible with GSM naming requirements.
// Returns a list of created GSM secret names and any error encountered.
//
// Parameters:
//   - ctx: Context for the operation
//   - gsmClient: GSM client for creating secrets
//   - collection: GSM collection name
//   - group: GSM group name
//   - cachedFields: Map of field name -> field value (from VaultCache)
//   - projectNumber: GCP project number
//   - dryRun: If true, only log what would be created without actually creating secrets
//   - dptp: If true, adds label "jira-project":"dptp" to created secrets
func MigrateVaultSecretToGSMFromCache(
	ctx context.Context,
	gsmClient gsmsecrets.SecretManagerClient,
	collection string,
	group string,
	cachedFields map[string]string,
	projectNumber string,
	dryRun bool,
	dptp bool,
) ([]string, error) {
	if len(cachedFields) == 0 {
		return nil, fmt.Errorf("no fields to migrate (cached fields is empty)")
	}

	logrus.Debugf("Creating %d GSM secret(s) from cached fields (collection=%s, group=%s)", len(cachedFields), collection, group)

	// Normalize group name for GSM
	normalizedGroup := gsmvalidation.NormalizeName(group)
	if normalizedGroup != group {
		logrus.Debugf("Normalized group name: %q -> %q", group, normalizedGroup)
	}

	var createdSecrets []string
	var errors []error

	// Migrate each field
	for fieldName, fieldValue := range cachedFields {
		normalizedField := gsmvalidation.NormalizeName(fieldName)
		if normalizedField != fieldName {
			logrus.Tracef("Normalized field name: %q -> %q", fieldName, normalizedField)
		}
		fullGsmSecretName := gsmsecrets.GetGSMSecretName(collection, normalizedGroup, normalizedField)

		// Validate secret name length
		if len(fullGsmSecretName) > gsmvalidation.GcpMaxNameLength {
			err := fmt.Errorf("GSM secret name too long (%d chars, max %d): %s",
				len(fullGsmSecretName), gsmvalidation.GcpMaxNameLength, fullGsmSecretName)
			logrus.WithError(err).Errorf("Skipping field %q", fieldName)
			errors = append(errors, err)
			continue
		}

		if dryRun {
			logrus.Debugf("[DRY RUN] Would create: %s (%d bytes)", fullGsmSecretName, len(fieldValue))
			createdSecrets = append(createdSecrets, fullGsmSecretName)
			continue
		}

		logrus.Debugf("Creating GSM secret: %s", fullGsmSecretName)
		//TODO: uncomment, temporary change for testing
		//annotations := map[string]string{
		//	"request-information": "created during the initial migration from Vault to GSM",
		//}
		//labels := map[string]string{
		//	"source":         "vault-migration",
		//	"migration-date": "2026-09-07",
		//}
		//if dptp {
		//	labels["jira-project"] = "dptp"
		//}
		//err := gsmsecrets.CreateOrUpdateSecret(
		//	ctx,
		//	gsmClient,
		//	projectNumber,
		//	fullGsmSecretName,
		//	[]byte(fieldValue),
		//	labels,
		//	annotations,
		//)
		//if err != nil {
		//	err = fmt.Errorf("failed to create GSM secret %q for field %q: %w", fullGsmSecretName, fieldName, err)
		//	logrus.WithError(err).Error("Migration failed for field")
		//	errors = append(errors, err)
		//	continue
		//}

		logrus.Infof("Successfully created: %s", fullGsmSecretName)
		createdSecrets = append(createdSecrets, fullGsmSecretName)
	}

	if len(errors) > 0 {
		return createdSecrets, fmt.Errorf("migration completed with %d errors out of %d fields", len(errors), len(cachedFields))
	}

	logrus.Debugf("Created %d GSM secret(s) in collection %q, group %q", len(cachedFields), collection, group)
	return createdSecrets, nil
}

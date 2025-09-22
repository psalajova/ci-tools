package vaultmigrationanalyzer

import (
	"fmt"
	"strings"

	gsmvalidation "github.com/openshift/ci-tools/pkg/gsm-validation"
	"github.com/openshift/ci-tools/pkg/vaultclient"
	"github.com/sirupsen/logrus"
)

const (
	SecretSyncTargetName      = "secretsync/target-name"
	SecretSyncTargetNamespace = "secretsync/target-namespace"
	SecretSyncTargetCluster   = "secretsync/target-cluster"
)

// AnalyzeVaultInventory performs Vault inventory scan and validation
func AnalyzeVaultInventory(vaultClient *vaultclient.VaultClient) (*VaultInventoryReport, []VaultSecret, error) {
	logrus.Info("Scanning Vault inventory...")

	report := &VaultInventoryReport{
		ValidationFailures: ValidationFailuresReport{
			UnsupportedCharsInNames:  []ValidationFailure{},
			UnsupportedCharsInFields: []ValidationFailure{},
			NameLengthExceeded:       []ValidationFailure{},
		},
		EmptyOrPlaceholder: []string{},
		UnexpectedPaths:    []string{},
	}

	var allSecrets []VaultSecret

	// Scan kv/dptp
	logrus.Info("Scanning kv/dptp...")
	dptpSecrets, err := scanVaultPath(vaultClient, "kv/dptp", report)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to scan kv/dptp: %w", err)
	}
	report.TotalDPTPSecrets = len(dptpSecrets)
	allSecrets = append(allSecrets, dptpSecrets...)

	// Scan kv/selfservice
	logrus.Info("Scanning kv/selfservice...")
	selfserviceSecrets, err := scanVaultPath(vaultClient, "kv/selfservice", report)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to scan kv/selfservice: %w", err)
	}
	report.TotalSelfserviceSecrets = len(selfserviceSecrets)
	allSecrets = append(allSecrets, selfserviceSecrets...)

	logrus.Infof("Total secrets scanned: %d (dptp: %d, selfservice: %d)",
		len(allSecrets), report.TotalDPTPSecrets, report.TotalSelfserviceSecrets)

	return report, allSecrets, nil
}

// scanVaultPath recursively scans a Vault path
func scanVaultPath(vaultClient *vaultclient.VaultClient, basePath string, report *VaultInventoryReport) ([]VaultSecret, error) {
	var secrets []VaultSecret

	// Determine if this is dptp or selfservice
	isDPTP := strings.HasPrefix(basePath, "kv/dptp")

	// List collections
	collections, err := vaultClient.ListKV(basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to list %s: %w", basePath, err)
	}

	for _, collection := range collections {
		// Remove trailing slash
		collection = strings.TrimSuffix(collection, "/")

		var collectionPath string
		if isDPTP {
			// DPTP secrets are at kv/dptp/secret-name (no nested structure)
			collectionPath = basePath + "/" + collection
		} else {
			// Selfservice has kv/selfservice/collection/group structure
			collectionPath = basePath + "/" + collection
		}

		if isDPTP {
			// For DPTP, collection itself is the secret
			secret, err := processSecret(vaultClient, basePath, collection, "", report)
			if err != nil {
				logrus.WithError(err).Warnf("Failed to process dptp secret %s", collection)
				continue
			}
			if secret != nil {
				secrets = append(secrets, *secret)
			}
		} else {
			// For selfservice, list groups within collection
			groups, err := vaultClient.ListKV(collectionPath)
			if err != nil {
				logrus.WithError(err).Warnf("Failed to list groups in %s", collectionPath)
				continue
			}

			for _, group := range groups {
				group = strings.TrimSuffix(group, "/")
				secret, err := processSecret(vaultClient, basePath, collection, group, report)
				if err != nil {
					logrus.WithError(err).Warnf("Failed to process secret %s/%s", collection, group)
					continue
				}
				if secret != nil {
					secrets = append(secrets, *secret)
				}
			}
		}
	}

	return secrets, nil
}

// processSecret processes a single Vault secret
func processSecret(vaultClient *vaultclient.VaultClient, basePath, collection, group string, report *VaultInventoryReport) (*VaultSecret, error) {
	var secretPath string
	if group == "" {
		// DPTP secret
		secretPath = basePath + "/" + collection
	} else {
		// Selfservice secret
		secretPath = basePath + "/" + collection + "/" + group
	}

	// Get secret data
	data, err := vaultClient.GetKV(secretPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get secret %s: %w", secretPath, err)
	}

	// Extract fields
	var fields []string
	var secretSyncMeta SecretSyncMetadata
	for key, value := range data.Data {
		// Check for secretsync metadata
		switch key {
		case SecretSyncTargetName:
			secretSyncMeta.TargetName = value
		case SecretSyncTargetNamespace:
			secretSyncMeta.TargetNamespace = value
		case SecretSyncTargetCluster:
			secretSyncMeta.TargetCluster = value
		default:
			fields = append(fields, key)
		}
	}

	secret := &VaultSecret{
		Path:           secretPath,
		Collection:     collection,
		Group:          group,
		Fields:         fields,
		FieldCount:     len(fields),
		SecretSyncMeta: secretSyncMeta,
	}

	// Check if empty or placeholder
	if len(fields) == 0 {
		secret.IsEmpty = true
		report.EmptyOrPlaceholder = append(report.EmptyOrPlaceholder, secretPath)
	} else if len(fields) == 1 && (fields[0] == "placeholder" || strings.Contains(secretPath, "placeholder")) {
		secret.IsPlaceholder = true
		report.EmptyOrPlaceholder = append(report.EmptyOrPlaceholder, secretPath)
	}

	// Validate names and fields
	validateSecret(secret, report)

	return secret, nil
}

// validateSecret validates collection, group, and field names
func validateSecret(secret *VaultSecret, report *VaultInventoryReport) {
	// Normalize and validate collection
	normalizedCollection := gsmvalidation.NormalizeName(secret.Collection)
	if normalizedCollection != secret.Collection {
		report.NormalizationApplied.DotsConverted++
		if strings.Contains(secret.Collection, "__") {
			report.NormalizationApplied.UnderscoresConverted++
		}
	}

	if !gsmvalidation.ValidateCollectionName(normalizedCollection) {
		// Find unsupported chars
		unsupportedChars := findUnsupportedChars(normalizedCollection)
		if len(unsupportedChars) > 0 {
			report.ValidationFailures.UnsupportedCharsInNames = append(
				report.ValidationFailures.UnsupportedCharsInNames,
				ValidationFailure{
					Path:             secret.Path,
					OriginalName:     secret.Collection,
					NormalizedName:   normalizedCollection,
					UnsupportedChars: unsupportedChars,
					Issue:            "Collection name contains unsupported characters",
				},
			)
		}
	}

	// Normalize and validate group (if exists)
	if secret.Group != "" {
		normalizedGroup := gsmvalidation.NormalizeName(secret.Group)
		if normalizedGroup != secret.Group {
			report.NormalizationApplied.DotsConverted++
			if strings.Contains(secret.Group, "__") {
				report.NormalizationApplied.UnderscoresConverted++
			}
		}

		if !gsmvalidation.ValidateGroupName(normalizedGroup) {
			unsupportedChars := findUnsupportedChars(normalizedGroup)
			if len(unsupportedChars) > 0 {
				report.ValidationFailures.UnsupportedCharsInNames = append(
					report.ValidationFailures.UnsupportedCharsInNames,
					ValidationFailure{
						Path:             secret.Path,
						OriginalName:     secret.Group,
						NormalizedName:   normalizedGroup,
						UnsupportedChars: unsupportedChars,
						Issue:            "Group name contains unsupported characters",
					},
				)
			}
		}
	}

	// Validate fields and check GSM secret name length
	for _, field := range secret.Fields {
		normalizedField := gsmvalidation.NormalizeName(field)
		if normalizedField != field {
			report.NormalizationApplied.DotsConverted++
			if strings.Contains(field, "__") {
				report.NormalizationApplied.UnderscoresConverted++
			}
		}

		if !gsmvalidation.ValidateSecretName(normalizedField) {
			unsupportedChars := findUnsupportedChars(normalizedField)
			if len(unsupportedChars) > 0 {
				report.ValidationFailures.UnsupportedCharsInFields = append(
					report.ValidationFailures.UnsupportedCharsInFields,
					ValidationFailure{
						Path:             secret.Path,
						Field:            field,
						OriginalName:     field,
						NormalizedName:   normalizedField,
						UnsupportedChars: unsupportedChars,
						Issue:            "Field name contains unsupported characters",
					},
				)
			}
		}

		// Check GSM secret name length: collection__group__field
		var gsmSecretName string
		if secret.Group == "" {
			// DPTP: collection__field
			gsmSecretName = normalizedCollection + gsmvalidation.CollectionSecretDelimiter + normalizedField
		} else {
			// Selfservice: collection__group__field
			gsmSecretName = normalizedCollection + gsmvalidation.CollectionSecretDelimiter +
				gsmvalidation.NormalizeName(secret.Group) + gsmvalidation.CollectionSecretDelimiter + normalizedField
		}

		if len(gsmSecretName) > gsmvalidation.GcpMaxNameLength {
			report.ValidationFailures.NameLengthExceeded = append(
				report.ValidationFailures.NameLengthExceeded,
				ValidationFailure{
					Path:             secret.Path,
					Field:            field,
					GSMSecretName:    gsmSecretName,
					GSMSecretLength:  len(gsmSecretName),
					MaxAllowedLength: gsmvalidation.GcpMaxNameLength,
					Issue:            fmt.Sprintf("GSM secret name exceeds maximum length by %d characters", len(gsmSecretName)-gsmvalidation.GcpMaxNameLength),
				},
			)
		}
	}
}

// findUnsupportedChars finds characters that are not supported after normalization
func findUnsupportedChars(name string) []rune {
	var unsupported []rune
	seenChars := make(map[rune]bool)

	for _, char := range name {
		// Check if char is supported (alphanumeric, dash, underscore, or normalized patterns)
		if !isAllowedChar(char) && !seenChars[char] {
			unsupported = append(unsupported, char)
			seenChars[char] = true
		}
	}

	return unsupported
}

// isAllowedChar checks if a character is allowed in GSM names (after normalization)
func isAllowedChar(char rune) bool {
	return (char >= 'a' && char <= 'z') ||
		(char >= 'A' && char <= 'Z') ||
		(char >= '0' && char <= '9') ||
		char == '-' || char == '_'
}

package gsmassmigration

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/config"
	gsmvalidation "github.com/openshift/ci-tools/pkg/gsm-validation"
)

// prowPodNamespace is the namespace ci-operator prow job pods run in (prow's
// pod_namespace). Container test secrets (the secrets:/secret: fields) mount K8s
// secrets by name from this namespace, so it disambiguates same-named secrets
// synced to multiple namespaces.
const prowPodNamespace = "ci"

// TargetedScope restricts credential-stanza rewriting to a single org/repo. A nil
// *TargetedScope means mass migration (the whole release repo is rewritten), which
// preserves the original behavior.
type TargetedScope struct {
	// ConfigSubdir limits config rewriting to ci-operator/config/<ConfigSubdir>
	// (e.g. "wildfly/wildfly-charts"). Empty means all configs.
	ConfigSubdir string
}

// UpdateCredentialStanzas walks CI operator configs and step registry to update credential references.
// Replaces namespace/name with collection/group for migrated Vault secrets,
// and namespace/name with bundle: for credentials matching gsm-config.yaml bundles.
// Returns a list of CredentialUpdate entries tracking what was changed.
//
// When scope is non-nil (targeted mode) config rewriting is limited to
// scope.ConfigSubdir, and the step-registry rewrite only touches stanzas that
// reference the migrated secrets (bundle references are left untouched, since a
// step-registry entry is shared by many repos). When scope is nil (mass mode)
// the whole release repo is rewritten and bundle references are converted too.
func UpdateCredentialStanzas(
	releaseRepoPath string,
	migrations []MigrationResult,
	skipCSIFlag bool,
	dryRun bool,
	scope *TargetedScope,
) ([]CredentialUpdate, error) {
	// Build mapping: secretName → (collection, group)
	credentialMap := buildCredentialMapping(migrations)

	// Load bundle info from gsm-config.yaml
	gsmConfigPath := path.Join(releaseRepoPath, "core-services/ci-secret-bootstrap/gsm-config.yaml")
	gsmContent, err := os.ReadFile(gsmConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read gsm-config.yaml: %w", err)
	}
	bundleNames := extractBundleNamesFromText(string(gsmContent))
	syncBundles := extractSyncBundleNames(gsmContent)

	configSubdir := ""
	if scope != nil {
		configSubdir = scope.ConfigSubdir
	}

	if scope != nil {
		logrus.Infof("Updating credential stanzas in targeted scope %q: %d migrated secrets, %d known bundles matched in configs only (step-registry limited to migrated secrets)", configSubdir, len(credentialMap), bundleNames.Len())
	} else {
		logrus.Infof("Updating credential stanzas for %d migrated secrets and %d known bundles (%d sync_to_cluster)", len(credentialMap), bundleNames.Len(), syncBundles.Len())
	}

	var allUpdates []CredentialUpdate

	// Update configs in ci-operator/config/* (scoped to configSubdir in targeted mode)
	configUpdates, err := updateConfigs(releaseRepoPath, configSubdir, credentialMap, bundleNames, syncBundles, skipCSIFlag, dryRun)
	if err != nil {
		return nil, fmt.Errorf("failed to update configs: %w", err)
	}
	allUpdates = append(allUpdates, configUpdates...)

	// Update ci-operator/step-registry/*. In targeted mode only migrated secrets are
	// rewritten (empty bundle sets), because a step-registry entry is shared across
	// repos and converting its bundle references would affect other repos.
	registryBundleNames, registrySyncBundles := bundleNames, syncBundles
	if scope != nil {
		registryBundleNames, registrySyncBundles = sets.New[string](), sets.New[string]()
	}
	registryUpdates, err := updateStepRegistry(releaseRepoPath, credentialMap, registryBundleNames, registrySyncBundles, dryRun)
	if err != nil {
		return nil, fmt.Errorf("failed to update step-registry: %w", err)
	}
	allUpdates = append(allUpdates, registryUpdates...)

	logrus.Infof("Updated %d credential stanzas across %d files", len(allUpdates), countUniqueFiles(allUpdates))
	return allUpdates, nil
}

// credentialKey is used to look up vault secrets by (name, namespace) pair
type credentialKey struct {
	name      string
	namespace string
}

// buildCredentialMapping creates a map from (K8s secret name, namespace) to (collection, group).
// A vault secret with comma-separated target namespaces creates one entry per namespace.
func buildCredentialMapping(migrations []MigrationResult) map[credentialKey]VaultSecretPath {
	mapping := make(map[credentialKey]VaultSecretPath)

	for _, migration := range migrations {
		if migration.Error != nil {
			continue
		}
		if migration.SecretName == "" {
			logrus.Warnf("Migration for %s succeeded but has no secret name", migration.VaultPath)
			continue
		}

		vsp := VaultSecretPath{
			FullPath:   migration.VaultPath,
			Collection: gsmvalidation.NormalizeName(migration.Collection),
			Group:      gsmvalidation.NormalizeName(migration.Group),
		}

		// Always add a name-only entry so container test secrets: lookups (which have no namespace) can find it
		mapping[credentialKey{name: migration.SecretName}] = vsp

		if migration.TargetNamespaces != "" {
			for _, ns := range strings.Split(migration.TargetNamespaces, ",") {
				ns = strings.TrimSpace(ns)
				if ns != "" {
					mapping[credentialKey{name: migration.SecretName, namespace: ns}] = vsp
				}
			}
		}
	}

	return mapping
}

// updateConfigs walks ci-operator/config/* and collects credential replacements, then applies them via text manipulation.
// configSubdir (e.g. "org/repo") restricts the walk to a subtree; empty means all configs.
func updateConfigs(releaseRepoPath, configSubdir string, credentialMap map[credentialKey]VaultSecretPath, bundleNames, syncBundles sets.Set[string], skipCSIFlag bool, dryRun bool) ([]CredentialUpdate, error) {
	configDir := path.Join(releaseRepoPath, "ci-operator", "config", configSubdir)
	var updates []CredentialUpdate

	callback := func(cfg *api.ReleaseBuildConfiguration, info *config.Info) error {
		fileUpdates, changed := updateConfigCredentials(cfg, credentialMap, bundleNames, syncBundles, skipCSIFlag, info.Filename)
		if !changed {
			return nil
		}
		updates = append(updates, fileUpdates...)
		logrus.Debugf("Found %d credentials to update in config: %s", len(fileUpdates), info.Filename)
		return nil
	}

	if err := config.OperateOnCIOperatorConfigDir(configDir, callback); err != nil {
		return nil, err
	}

	if len(updates) > 0 {
		if err := applyTextReplacements(updates, dryRun); err != nil {
			return nil, fmt.Errorf("failed to apply text replacements: %w", err)
		}
	}

	return updates, nil
}

// updateConfigCredentials updates credentials in a single ReleaseBuildConfiguration
func updateConfigCredentials(cfg *api.ReleaseBuildConfiguration, credentialMap map[credentialKey]VaultSecretPath, bundleNames, syncBundles sets.Set[string], skipCSIFlag bool, filePath string) ([]CredentialUpdate, bool) {
	var updates []CredentialUpdate
	changed := false

	// Process all tests for credentials
	for i := range cfg.Tests {
		test := &cfg.Tests[i]

		// MultiStageTestConfiguration (reference-based)
		if test.MultiStageTestConfiguration != nil {
			testUpdates, testChanged := processMultiStageCredentials(test.MultiStageTestConfiguration, credentialMap, bundleNames, syncBundles, filePath)
			if testChanged {
				updates = append(updates, testUpdates...)
				changed = true
			}
		}

		// MultiStageTestConfigurationLiteral (inline)
		if test.MultiStageTestConfigurationLiteral != nil {
			testUpdates, testChanged := processMultiStageLiteralCredentials(test.MultiStageTestConfigurationLiteral, credentialMap, bundleNames, syncBundles, filePath)
			if testChanged {
				updates = append(updates, testUpdates...)
				changed = true
			}
		}
	}

	// Container test secrets (Secret and Secrets fields)
	for i := range cfg.Tests {
		test := &cfg.Tests[i]

		var secrets []*api.Secret
		if test.Secret != nil {
			secrets = append(secrets, test.Secret)
		}
		secrets = append(secrets, test.Secrets...)

		secretUpdates, secretsChanged := updateSecrets(secrets, credentialMap, bundleNames, syncBundles, filePath)
		if secretsChanged {
			updates = append(updates, secretUpdates...)
			changed = true
		}
	}

	if changed && !skipCSIFlag {
		if cfg.Prowgen == nil {
			cfg.Prowgen = &api.ProwgenOverrides{}
		}
		cfg.Prowgen.EnableSecretsStoreCSIDriver = true
	}

	return updates, changed
}

// processMultiStageCredentials processes credentials in MultiStageTestConfiguration (reference-based)
func processMultiStageCredentials(multiStage *api.MultiStageTestConfiguration, credentialMap map[credentialKey]VaultSecretPath, bundleNames, syncBundles sets.Set[string], filePath string) ([]CredentialUpdate, bool) {
	var updates []CredentialUpdate
	changed := false

	for i := range multiStage.Pre {
		stepUpdates, stepChanged := processStepCredentials(&multiStage.Pre[i], credentialMap, bundleNames, syncBundles, filePath)
		if stepChanged {
			updates = append(updates, stepUpdates...)
			changed = true
		}
	}

	for i := range multiStage.Test {
		stepUpdates, stepChanged := processStepCredentials(&multiStage.Test[i], credentialMap, bundleNames, syncBundles, filePath)
		if stepChanged {
			updates = append(updates, stepUpdates...)
			changed = true
		}
	}

	for i := range multiStage.Post {
		stepUpdates, stepChanged := processStepCredentials(&multiStage.Post[i], credentialMap, bundleNames, syncBundles, filePath)
		if stepChanged {
			updates = append(updates, stepUpdates...)
			changed = true
		}
	}

	return updates, changed
}

// processMultiStageLiteralCredentials processes credentials in MultiStageTestConfigurationLiteral (inline)
func processMultiStageLiteralCredentials(multiStage *api.MultiStageTestConfigurationLiteral, credentialMap map[credentialKey]VaultSecretPath, bundleNames, syncBundles sets.Set[string], filePath string) ([]CredentialUpdate, bool) {
	var updates []CredentialUpdate
	changed := false

	for i := range multiStage.Pre {
		stepUpdates, stepChanged := processLiteralStepCredentials(&multiStage.Pre[i], credentialMap, bundleNames, syncBundles, filePath)
		if stepChanged {
			updates = append(updates, stepUpdates...)
			changed = true
		}
	}

	for i := range multiStage.Test {
		stepUpdates, stepChanged := processLiteralStepCredentials(&multiStage.Test[i], credentialMap, bundleNames, syncBundles, filePath)
		if stepChanged {
			updates = append(updates, stepUpdates...)
			changed = true
		}
	}

	for i := range multiStage.Post {
		stepUpdates, stepChanged := processLiteralStepCredentials(&multiStage.Post[i], credentialMap, bundleNames, syncBundles, filePath)
		if stepChanged {
			updates = append(updates, stepUpdates...)
			changed = true
		}
	}

	return updates, changed
}

// processStepCredentials processes credentials in a TestStep (reference-based)
func processStepCredentials(step *api.TestStep, credentialMap map[credentialKey]VaultSecretPath, bundleNames, syncBundles sets.Set[string], filePath string) ([]CredentialUpdate, bool) {
	if step.LiteralTestStep == nil || len(step.LiteralTestStep.Credentials) == 0 {
		return nil, false
	}

	return updateCredentials(step.LiteralTestStep.Credentials, credentialMap, bundleNames, syncBundles, filePath)
}

// processLiteralStepCredentials processes credentials in a LiteralTestStep (inline)
func processLiteralStepCredentials(step *api.LiteralTestStep, credentialMap map[credentialKey]VaultSecretPath, bundleNames, syncBundles sets.Set[string], filePath string) ([]CredentialUpdate, bool) {
	if len(step.Credentials) == 0 {
		return nil, false
	}

	return updateCredentials(step.Credentials, credentialMap, bundleNames, syncBundles, filePath)
}

// updateCredentials identifies credential entries that need migrating.
// Does NOT modify the struct -- just collects what needs to change for text-based replacement.
func updateCredentials(credentials []api.CredentialReference, credentialMap map[credentialKey]VaultSecretPath, bundleNames, syncBundles sets.Set[string], filePath string) ([]CredentialUpdate, bool) {
	var updates []CredentialUpdate
	changed := false

	for _, cred := range credentials {
		if (cred.Collection != "" && cred.Group != "") || cred.Bundle != "" {
			continue
		}

		// Bundle match
		if cred.Name != "" && bundleNames.Has(cred.Name) {
			updates = append(updates, CredentialUpdate{
				FilePath:      filePath,
				OldNamespace:  cred.Namespace,
				OldName:       cred.Name,
				Bundle:        cred.Name,
				KeepNamespace: syncBundles.Has(cred.Name),
			})
			changed = true
			logrus.Debugf("Will convert credential to bundle reference: %s in %s", cred.Name, filePath)
			continue
		}

		// Vault secret match -- look up by (name, namespace) first, fall back to name-only
		vaultSecret, exists := credentialMap[credentialKey{name: cred.Name, namespace: cred.Namespace}]
		if !exists {
			vaultSecret, exists = credentialMap[credentialKey{name: cred.Name}]
		}
		if !exists {
			if cred.Name != "" && cred.Namespace != "" {
				logrus.Debugf("UNMIGRATED credential: name=%s namespace=%s mount_path=%s file=%s (no matching Vault secret found)",
					cred.Name, cred.Namespace, cred.MountPath, filePath)
			}
			continue
		}

		updates = append(updates, CredentialUpdate{
			FilePath:      filePath,
			OldNamespace:  cred.Namespace,
			OldName:       cred.Name,
			NewCollection: vaultSecret.Collection,
			NewGroup:      vaultSecret.Group,
		})
		changed = true
		logrus.Tracef("Will update credential: %s/%s -> %s/%s in %s", cred.Namespace, cred.Name, vaultSecret.Collection, vaultSecret.Group, filePath)
	}

	return updates, changed
}

// updateSecrets identifies container test secret entries that need migrating.
// Similar to updateCredentials but for api.Secret (no namespace field).
func updateSecrets(secrets []*api.Secret, credentialMap map[credentialKey]VaultSecretPath, bundleNames, syncBundles sets.Set[string], filePath string) ([]CredentialUpdate, bool) {
	var updates []CredentialUpdate
	changed := false

	for _, secret := range secrets {
		if secret == nil {
			continue
		}
		if (secret.Collection != "" && secret.Group != "") || secret.Bundle != "" {
			continue
		}
		if secret.Name == "" {
			continue
		}

		// Bundle match
		if bundleNames.Has(secret.Name) {
			updates = append(updates, CredentialUpdate{
				FilePath: filePath,
				OldName:  secret.Name,
				Bundle:   secret.Name,
				IsSecret: true,
			})
			changed = true
			logrus.Debugf("Will convert secret to bundle reference: %s in %s", secret.Name, filePath)
			continue
		}

		// Vault secret match. Container test secrets have no namespace: the K8s secret
		// is read from the namespace the ci-operator prow job pod runs in (prow's
		// pod_namespace, "ci"). When the same K8s name is synced to multiple namespaces
		// by different Vault secrets (e.g. an "-ci" variant), the "ci" copy is what the
		// job actually mounts, so prefer it. Fall back to a name-only lookup for secrets
		// synced to a single namespace.
		vaultSecret, exists := credentialMap[credentialKey{name: secret.Name, namespace: prowPodNamespace}]
		if !exists {
			vaultSecret, exists = credentialMap[credentialKey{name: secret.Name}]
		}
		if !exists {
			logrus.Debugf("UNMIGRATED secret: name=%s mount_path=%s file=%s (no matching Vault secret found)",
				secret.Name, secret.MountPath, filePath)
			continue
		}

		updates = append(updates, CredentialUpdate{
			FilePath:      filePath,
			OldName:       secret.Name,
			NewCollection: vaultSecret.Collection,
			NewGroup:      vaultSecret.Group,
			IsSecret:      true,
		})
		changed = true
		logrus.Tracef("Will update secret: %s -> %s/%s in %s", secret.Name, vaultSecret.Collection, vaultSecret.Group, filePath)
	}

	return updates, changed
}

// NormalizeStepRegistry walks step-registry files and re-serializes them without
// modifying any content. This normalizes YAML formatting (key ordering, indentation)
// so that subsequent credential updates produce minimal diffs.
func NormalizeStepRegistry(releaseRepoPath string, dryRun bool) (int, error) {
	registryPath := path.Join(releaseRepoPath, "ci-operator", "step-registry")
	normalized := 0

	err := filepath.WalkDir(registryPath, func(filePath string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Name() == "OWNERS" || strings.HasPrefix(d.Name(), "..") {
			return nil
		}

		var config any
		var configType string

		switch {
		case strings.HasSuffix(filePath, "-ref.yaml"):
			var c api.RegistryReferenceConfig
			if err := unmarshalFile(filePath, &c); err != nil {
				return nil
			}
			config = &c
			configType = "ref"
		case strings.HasSuffix(filePath, "-chain.yaml"):
			var c api.RegistryChainConfig
			if err := unmarshalFile(filePath, &c); err != nil {
				return nil
			}
			config = &c
			configType = "chain"
		case strings.HasSuffix(filePath, "-workflow.yaml"):
			var c api.RegistryWorkflowConfig
			if err := unmarshalFile(filePath, &c); err != nil {
				return nil
			}
			config = &c
			configType = "workflow"
		default:
			return nil
		}

		original, _ := os.ReadFile(filePath)
		marshaled, err := yaml.Marshal(config)
		if err != nil {
			logrus.WithError(err).Warnf("Failed to marshal %s", filePath)
			return nil
		}

		if string(original) == string(marshaled) {
			return nil
		}

		normalized++
		if dryRun {
			logrus.Debugf("[DRY-RUN] Would normalize %s: %s", configType, filePath)
			return nil
		}

		if err := os.WriteFile(filePath, marshaled, 0644); err != nil {
			logrus.WithError(err).Warnf("Failed to write %s", filePath)
		}
		return nil
	})

	logrus.Infof("Normalized %d step-registry files", normalized)
	return normalized, err
}

// AddCSIFlag walks all ci-operator configs and sets EnableSecretsStoreCSIDriver: true
// on every config unconditionally. Post-migration, all repos use GSM.
// AddCSIFlag walks all ci-operator configs and sets EnableSecretsStoreCSIDriver: true
// on every config unconditionally. Post-migration, all repos use GSM.
// This is intentionally broad -- step-registry refs may have credentials that aren't
// visible in the config itself. The flag will be removed post-migration once prowgen
// auto-detects credential usage.
func AddCSIFlag(releaseRepoPath string, dryRun bool) (int, error) {
	configDir := path.Join(releaseRepoPath, "ci-operator", "config")
	modified := 0

	callback := func(cfg *api.ReleaseBuildConfiguration, info *config.Info) error {
		if cfg.Prowgen != nil && cfg.Prowgen.EnableSecretsStoreCSIDriver {
			return nil
		}

		if cfg.Prowgen == nil {
			cfg.Prowgen = &api.ProwgenOverrides{}
		}
		cfg.Prowgen.EnableSecretsStoreCSIDriver = true
		modified++

		if dryRun {
			logrus.Debugf("[DRY-RUN] Would add CSI flag to %s", info.Filename)
			return nil
		}
		return writeConfigToFile(cfg, info.Filename)
	}

	if err := config.OperateOnCIOperatorConfigDir(configDir, callback); err != nil {
		return 0, err
	}

	logrus.Infof("Added CSI flag to %d configs", modified)
	return modified, nil
}

func unmarshalFile(filePath string, target any) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	return yaml.UnmarshalStrict(content, target)
}

// updateStepRegistry walks ci-operator/step-registry/* and updates credential stanzas
func updateStepRegistry(releaseRepoPath string, credentialMap map[credentialKey]VaultSecretPath, bundleNames, syncBundles sets.Set[string], dryRun bool) ([]CredentialUpdate, error) {
	registryPath := path.Join(releaseRepoPath, "ci-operator", "step-registry")
	var updates []CredentialUpdate

	err := filepath.WalkDir(registryPath, func(filePath string, d fs.DirEntry, err error) error {
		if err != nil {
			logrus.WithError(err).Warnf("Failed to walk %s", filePath)
			return nil
		}
		if d.IsDir() {
			return nil
		}

		// Skip non-YAML files and OWNERS files
		if d.Name() == "OWNERS" || strings.HasPrefix(d.Name(), "..") {
			return nil
		}
		if !strings.HasSuffix(filePath, ".yaml") && !strings.HasSuffix(filePath, ".yml") {
			return nil
		}

		// Process different step-registry file types
		if strings.HasSuffix(filePath, "-ref.yaml") {
			fileUpdates, err := processReferenceFile(filePath, credentialMap, bundleNames, syncBundles, dryRun)
			if err != nil {
				logrus.WithError(err).Warnf("Failed to process reference file %s", filePath)
				return nil
			}
			updates = append(updates, fileUpdates...)
		} else if strings.HasSuffix(filePath, "-chain.yaml") {
			fileUpdates, err := processChainFile(filePath, credentialMap, bundleNames, syncBundles, dryRun)
			if err != nil {
				logrus.WithError(err).Warnf("Failed to process chain file %s", filePath)
				return nil
			}
			updates = append(updates, fileUpdates...)
		} else if strings.HasSuffix(filePath, "-workflow.yaml") {
			fileUpdates, err := processWorkflowFile(filePath, credentialMap, bundleNames, syncBundles, dryRun)
			if err != nil {
				logrus.WithError(err).Warnf("Failed to process workflow file %s", filePath)
				return nil
			}
			updates = append(updates, fileUpdates...)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	if len(updates) > 0 {
		if err := applyTextReplacements(updates, dryRun); err != nil {
			return nil, fmt.Errorf("failed to apply text replacements: %w", err)
		}
	}

	return updates, nil
}

// processReferenceFile processes a step registry reference file (-ref.yaml)
func processReferenceFile(filePath string, credentialMap map[credentialKey]VaultSecretPath, bundleNames, syncBundles sets.Set[string], _ bool) ([]CredentialUpdate, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var stepConfig api.RegistryReferenceConfig
	if err := yaml.UnmarshalStrict(content, &stepConfig); err != nil {
		return nil, fmt.Errorf("failed to unmarshal: %w", err)
	}

	if len(stepConfig.Reference.Credentials) == 0 {
		return nil, nil
	}

	fileUpdates, changed := updateCredentials(stepConfig.Reference.Credentials, credentialMap, bundleNames, syncBundles, filePath)
	if !changed {
		return nil, nil
	}

	return fileUpdates, nil
}

// processChainFile processes a step registry chain file (-chain.yaml)
func processChainFile(filePath string, credentialMap map[credentialKey]VaultSecretPath, bundleNames, syncBundles sets.Set[string], _ bool) ([]CredentialUpdate, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var chainConfig api.RegistryChainConfig
	if err := yaml.UnmarshalStrict(content, &chainConfig); err != nil {
		return nil, fmt.Errorf("failed to unmarshal: %w", err)
	}

	var fileUpdates []CredentialUpdate
	changed := false

	for _, step := range chainConfig.Chain.Steps {
		if step.LiteralTestStep != nil && len(step.LiteralTestStep.Credentials) > 0 {
			stepUpdates, stepChanged := updateCredentials(step.LiteralTestStep.Credentials, credentialMap, bundleNames, syncBundles, filePath)
			if stepChanged {
				fileUpdates = append(fileUpdates, stepUpdates...)
				changed = true
			}
		}
	}

	if !changed {
		return nil, nil
	}

	return fileUpdates, nil
}

// processWorkflowFile processes a step registry workflow file (-workflow.yaml)
func processWorkflowFile(filePath string, credentialMap map[credentialKey]VaultSecretPath, bundleNames, syncBundles sets.Set[string], _ bool) ([]CredentialUpdate, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var workflowConfig api.RegistryWorkflowConfig
	if err := yaml.UnmarshalStrict(content, &workflowConfig); err != nil {
		return nil, fmt.Errorf("failed to unmarshal: %w", err)
	}

	fileUpdates, changed := processMultiStageCredentials(&workflowConfig.Workflow.Steps, credentialMap, bundleNames, syncBundles, filePath)
	if !changed {
		return nil, nil
	}

	return fileUpdates, nil
}

// applyTextReplacements applies credential replacements to files using raw text manipulation.
// Preserves original formatting -- only changes the specific credential fields.
func applyTextReplacements(updates []CredentialUpdate, dryRun bool) error {
	byFile := make(map[string][]CredentialUpdate)
	for _, u := range updates {
		byFile[u.FilePath] = append(byFile[u.FilePath], u)
	}

	for filePath, fileUpdates := range byFile {
		content, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", filePath, err)
		}

		lines := strings.Split(string(content), "\n")
		modified := false

		// Process each update independently, re-scanning from scratch each time
		// to avoid index shift issues when multiple credentials are in one file
		for _, update := range fileUpdates {
			applied := false

			if update.IsSecret {
				lines, applied = applySecretReplacement(lines, update)
			} else {
				lines, applied = applyCredentialReplacement(lines, update)
			}

			if applied {
				modified = true
			} else {
				if update.IsSecret {
					logrus.Warnf("Could not find secret name=%s in %s for text replacement", update.OldName, filePath)
				} else {
					logrus.Warnf("Could not find credential name=%s namespace=%s in %s for text replacement", update.OldName, update.OldNamespace, filePath)
				}
			}
		}

		if modified && !dryRun {
			if err := os.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
				return fmt.Errorf("failed to write %s: %w", filePath, err)
			}
		}
	}

	return nil
}

// applyCredentialReplacement handles text replacement for a credentials: entry (has namespace).
func applyCredentialReplacement(lines []string, update CredentialUpdate) ([]string, bool) {
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		trimmedLine := stripInlineComment(trimmed)
		if trimmedLine != "name: "+update.OldName && trimmedLine != "- name: "+update.OldName {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		nsLineIdx := -1
		mountPathFound := false
		closestDist := 999
		for j := max(0, i-3); j <= min(len(lines)-1, i+3); j++ {
			if j == i {
				continue
			}
			t := stripInlineComment(strings.TrimSpace(lines[j]))
			if (t == "namespace: "+update.OldNamespace || t == "- namespace: "+update.OldNamespace) && !strings.HasPrefix(t, "#") {
				dist := i - j
				if dist < 0 {
					dist = -dist
				}
				if dist < closestDist {
					closestDist = dist
					nsLineIdx = j
				}
			}
			if strings.HasPrefix(t, "mount_path:") || strings.HasPrefix(t, "- mount_path:") {
				mountPathFound = true
			}
		}

		if nsLineIdx < 0 || !mountPathFound {
			continue
		}

		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		nsIndent := lines[nsLineIdx][:len(lines[nsLineIdx])-len(strings.TrimLeft(lines[nsLineIdx], " "))]

		if update.Bundle != "" {
			nsTrimmed := stripInlineComment(strings.TrimSpace(lines[nsLineIdx]))
			if update.KeepNamespace {
				// sync_to_cluster bundle: replace name with bundle, keep namespace (strip comment)
				if strings.HasPrefix(trimmed, "- ") {
					lines[i] = indent + "- bundle: " + update.Bundle
					lines[nsLineIdx] = nsIndent + "namespace: " + update.OldNamespace
				} else {
					lines[i] = indent + "bundle: " + update.Bundle
					if strings.HasPrefix(nsTrimmed, "- ") {
						lines[nsLineIdx] = nsIndent + "- namespace: " + update.OldNamespace
					} else {
						lines[nsLineIdx] = nsIndent + "namespace: " + update.OldNamespace
					}
				}
			} else {
				// non-sync bundle: replace name with bundle, remove namespace line
				if strings.HasPrefix(trimmed, "- ") {
					lines[i] = indent + "- bundle: " + update.Bundle
				} else if strings.HasPrefix(nsTrimmed, "- ") {
					lines[i] = nsIndent + "- bundle: " + update.Bundle
				} else {
					lines[i] = indent + "bundle: " + update.Bundle
				}
				lines = append(lines[:nsLineIdx], lines[nsLineIdx+1:]...)
			}
		} else {
			nsTrimmed := strings.TrimSpace(lines[nsLineIdx])
			if strings.HasPrefix(nsTrimmed, "- ") {
				lines[nsLineIdx] = nsIndent + "- collection: " + update.NewCollection
				lines[i] = indent + "group: " + update.NewGroup
			} else if strings.HasPrefix(trimmed, "- ") {
				lines[i] = indent + "- collection: " + update.NewCollection
				lines[nsLineIdx] = nsIndent + "group: " + update.NewGroup
			} else {
				lines[nsLineIdx] = nsIndent + "collection: " + update.NewCollection
				lines[i] = indent + "group: " + update.NewGroup
			}
		}

		logrus.Debugf("Applied credential replacement: %s/%s", update.OldName, update.OldNamespace)
		return lines, true
	}

	return lines, false
}

// applySecretReplacement handles text replacement for a secrets: entry (no namespace).
func applySecretReplacement(lines []string, update CredentialUpdate) ([]string, bool) {
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		trimmedLine := stripInlineComment(trimmed)
		if trimmedLine != "name: "+update.OldName && trimmedLine != "- name: "+update.OldName {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Confirm this is a secrets: entry: mount_path nearby, NO namespace nearby
		mountPathFound := false
		namespaceFound := false
		for j := max(0, i-3); j <= min(len(lines)-1, i+3); j++ {
			if j == i {
				continue
			}
			t := strings.TrimSpace(lines[j])
			if strings.HasPrefix(t, "mount_path:") || strings.HasPrefix(t, "- mount_path:") {
				mountPathFound = true
			}
			if strings.HasPrefix(t, "namespace:") || strings.HasPrefix(t, "- namespace:") {
				namespaceFound = true
			}
		}

		if !mountPathFound || namespaceFound {
			continue
		}

		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]

		if update.Bundle != "" {
			if strings.HasPrefix(trimmed, "- ") {
				lines[i] = indent + "- bundle: " + update.Bundle
			} else {
				lines[i] = indent + "bundle: " + update.Bundle
			}
		} else {
			if strings.HasPrefix(trimmed, "- ") {
				lines[i] = indent + "- collection: " + update.NewCollection
				groupLine := indent + "  group: " + update.NewGroup
				lines = append(lines[:i+1], append([]string{groupLine}, lines[i+1:]...)...)
			} else {
				lines[i] = indent + "collection: " + update.NewCollection
				groupLine := indent + "group: " + update.NewGroup
				lines = append(lines[:i+1], append([]string{groupLine}, lines[i+1:]...)...)
			}
		}

		logrus.Debugf("Applied secret replacement: %s", update.OldName)
		return lines, true
	}

	return lines, false
}

// extractSyncBundleNames parses gsm-config.yaml to find bundles with sync_to_cluster: true
func extractSyncBundleNames(content []byte) sets.Set[string] {
	syncBundles := sets.New[string]()
	var gsmConfig api.GSMConfig
	if err := yaml.Unmarshal(content, &gsmConfig); err != nil {
		logrus.WithError(err).Warn("Failed to parse gsm-config.yaml for sync_to_cluster detection, treating all bundles as non-sync")
		return syncBundles
	}
	for _, bundle := range gsmConfig.Bundles {
		if bundle.SyncToCluster {
			syncBundles.Insert(bundle.Name)
		}
	}
	return syncBundles
}

func stripInlineComment(s string) string {
	if idx := strings.Index(s, " #"); idx >= 0 {
		return strings.TrimRight(s[:idx], " ")
	}
	return strings.TrimRight(s, " ")
}

// writeConfigToFile writes a ReleaseBuildConfiguration back to a YAML file
func writeConfigToFile(cfg *api.ReleaseBuildConfiguration, filename string) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		return fmt.Errorf("failed to write config to %s: %w", filename, err)
	}

	return nil
}

// writeRegistryFile writes a step registry configuration back to a YAML file
func writeRegistryFile(config any, filename string) error {
	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal registry config: %w", err)
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		return fmt.Errorf("failed to write registry file %s: %w", filename, err)
	}

	return nil
}

// printYAMLDiff prints a unified diff between original file and updated config
func printYAMLDiff(filePath string, updatedConfig any) error {
	// Read original file
	originalContent, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read original file: %w", err)
	}

	// Marshal updated config to YAML
	updatedContent, err := yaml.Marshal(updatedConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal updated config: %w", err)
	}

	// Generate unified diff
	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(originalContent)),
		B:        difflib.SplitLines(string(updatedContent)),
		FromFile: filePath,
		ToFile:   filePath,
		Context:  3,
	}

	diffText, err := difflib.GetUnifiedDiffString(diff)
	if err != nil {
		return fmt.Errorf("failed to generate diff: %w", err)
	}

	if diffText != "" {
		fmt.Printf("\n--- %s\n", filePath)
		fmt.Print(diffText)
	}

	return nil
}

// StaleCredential represents an old-format credential that wasn't migrated
type StaleCredential struct {
	FilePath  string
	Name      string
	Namespace string
}

// RemoveStaleCredentials scans configs and step-registry for old-format credentials (namespace/name)
// that don't exist in the Vault cache or as bundles in gsm-config.yaml.
// Credentials matching a bundle are converted to bundle: format.
// Credentials matching neither Vault nor bundles are removed.
func RemoveStaleCredentials(releaseRepoPath string, cache *VaultCache, dryRun bool) ([]StaleCredential, error) {
	// Build set of known secret names from Vault cache
	knownSecrets := make(map[string]bool)
	for _, secret := range cache.Secrets {
		if secret.TargetName != "" {
			knownSecrets[secret.TargetName] = true
		}
		// DPTP secrets don't have target-name; use group name instead
		if secret.Collection == "test-platform-infra" {
			knownSecrets[secret.Group] = true
		}
	}

	// Load bundle names from gsm-config.yaml
	gsmConfigPath := path.Join(releaseRepoPath, "core-services/ci-secret-bootstrap/gsm-config.yaml")
	gsmContent, err := os.ReadFile(gsmConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read gsm-config.yaml: %w", err)
	}
	bundleNames := extractBundleNamesFromText(string(gsmContent))
	logrus.Infof("Vault cache has %d known secret target-names, gsm-config.yaml has %d bundles", len(knownSecrets), bundleNames.Len())

	var allStale []StaleCredential

	// Scan configs
	configDir := path.Join(releaseRepoPath, "ci-operator", "config")
	configCallback := func(cfg *api.ReleaseBuildConfiguration, info *config.Info) error {
		stale, changed := removeStaleFromConfig(cfg, knownSecrets, bundleNames, info.Filename)
		if !changed {
			return nil
		}
		allStale = append(allStale, stale...)
		if !dryRun {
			return writeConfigToFile(cfg, info.Filename)
		}
		return nil
	}
	if err := config.OperateOnCIOperatorConfigDir(configDir, configCallback); err != nil {
		return nil, fmt.Errorf("failed to scan configs: %w", err)
	}

	// Scan step-registry
	registryPath := path.Join(releaseRepoPath, "ci-operator", "step-registry")
	if err := filepath.WalkDir(registryPath, func(filePath string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Name() == "OWNERS" || strings.HasPrefix(d.Name(), "..") {
			return nil
		}

		switch {
		case strings.HasSuffix(filePath, "-ref.yaml"):
			removeStaleFromRefFile(filePath, knownSecrets, bundleNames, &allStale, dryRun)
		case strings.HasSuffix(filePath, "-chain.yaml"):
			removeStaleFromChainFile(filePath, knownSecrets, bundleNames, &allStale, dryRun)
		case strings.HasSuffix(filePath, "-workflow.yaml"):
			removeStaleFromWorkflowFile(filePath, knownSecrets, bundleNames, &allStale, dryRun)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to scan step-registry: %w", err)
	}

	logrus.Infof("Found %d stale credentials across %d files", len(allStale), countStaleFiles(allStale))
	if len(allStale) > 0 {
		for _, s := range allStale {
			logrus.Infof("  Stale: %s/%s in %s", s.Namespace, s.Name, s.FilePath)
		}
	}

	return allStale, nil
}

func removeStaleFromConfig(cfg *api.ReleaseBuildConfiguration, knownSecrets map[string]bool, bundleNames sets.Set[string], filePath string) ([]StaleCredential, bool) {
	var stale []StaleCredential
	changed := false

	for i := range cfg.Tests {
		test := &cfg.Tests[i]
		if test.MultiStageTestConfiguration != nil {
			s, c := removeStaleFromMultiStage(test.MultiStageTestConfiguration, knownSecrets, bundleNames, filePath)
			stale = append(stale, s...)
			if c {
				changed = true
			}
		}
		if test.MultiStageTestConfigurationLiteral != nil {
			s, c := removeStaleFromMultiStageLiteral(test.MultiStageTestConfigurationLiteral, knownSecrets, bundleNames, filePath)
			stale = append(stale, s...)
			if c {
				changed = true
			}
		}
	}

	return stale, changed
}

func removeStaleFromMultiStage(ms *api.MultiStageTestConfiguration, knownSecrets map[string]bool, bundleNames sets.Set[string], filePath string) ([]StaleCredential, bool) {
	var stale []StaleCredential
	changed := false
	for i := range ms.Pre {
		if ms.Pre[i].LiteralTestStep != nil {
			s, c := filterStaleCredentials(&ms.Pre[i].LiteralTestStep.Credentials, knownSecrets, bundleNames, filePath)
			stale = append(stale, s...)
			if c {
				changed = true
			}
		}
	}
	for i := range ms.Test {
		if ms.Test[i].LiteralTestStep != nil {
			s, c := filterStaleCredentials(&ms.Test[i].LiteralTestStep.Credentials, knownSecrets, bundleNames, filePath)
			stale = append(stale, s...)
			if c {
				changed = true
			}
		}
	}
	for i := range ms.Post {
		if ms.Post[i].LiteralTestStep != nil {
			s, c := filterStaleCredentials(&ms.Post[i].LiteralTestStep.Credentials, knownSecrets, bundleNames, filePath)
			stale = append(stale, s...)
			if c {
				changed = true
			}
		}
	}
	return stale, changed
}

func removeStaleFromMultiStageLiteral(ms *api.MultiStageTestConfigurationLiteral, knownSecrets map[string]bool, bundleNames sets.Set[string], filePath string) ([]StaleCredential, bool) {
	var stale []StaleCredential
	changed := false
	for i := range ms.Pre {
		s, c := filterStaleCredentials(&ms.Pre[i].Credentials, knownSecrets, bundleNames, filePath)
		stale = append(stale, s...)
		if c {
			changed = true
		}
	}
	for i := range ms.Test {
		s, c := filterStaleCredentials(&ms.Test[i].Credentials, knownSecrets, bundleNames, filePath)
		stale = append(stale, s...)
		if c {
			changed = true
		}
	}
	for i := range ms.Post {
		s, c := filterStaleCredentials(&ms.Post[i].Credentials, knownSecrets, bundleNames, filePath)
		stale = append(stale, s...)
		if c {
			changed = true
		}
	}
	return stale, changed
}

func filterStaleCredentials(credentials *[]api.CredentialReference, knownSecrets map[string]bool, bundleNames sets.Set[string], filePath string) ([]StaleCredential, bool) {
	var stale []StaleCredential
	var kept []api.CredentialReference
	changed := false

	for _, cred := range *credentials {
		// Already migrated -- keep
		if (cred.Collection != "" && cred.Group != "") || cred.Bundle != "" {
			kept = append(kept, cred)
			continue
		}
		// Check if it's a bundle in gsm-config.yaml
		if cred.Name != "" && bundleNames.Has(cred.Name) {
			logrus.Debugf("Converting %s to bundle reference in %s", cred.Name, filePath)
			cred.Bundle = cred.Name
			cred.Name = ""
			cred.Namespace = ""
			kept = append(kept, cred)
			changed = true
			continue
		}
		// Check if it exists in Vault
		if cred.Name != "" && knownSecrets[cred.Name] {
			kept = append(kept, cred)
			continue
		}
		// Not in Vault, not a bundle -- stale
		stale = append(stale, StaleCredential{
			FilePath:  filePath,
			Name:      cred.Name,
			Namespace: cred.Namespace,
		})
		changed = true
	}

	if changed {
		*credentials = kept
	}
	return stale, changed
}

func removeStaleFromRefFile(filePath string, knownSecrets map[string]bool, bundleNames sets.Set[string], allStale *[]StaleCredential, dryRun bool) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return
	}
	var stepConfig api.RegistryReferenceConfig
	if err := yaml.UnmarshalStrict(content, &stepConfig); err != nil {
		return
	}
	stale, changed := filterStaleCredentials(&stepConfig.Reference.Credentials, knownSecrets, bundleNames, filePath)
	if !changed {
		return
	}
	*allStale = append(*allStale, stale...)
	if !dryRun {
		writeRegistryFile(&stepConfig, filePath)
	}
}

func removeStaleFromChainFile(filePath string, knownSecrets map[string]bool, bundleNames sets.Set[string], allStale *[]StaleCredential, dryRun bool) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return
	}
	var chainConfig api.RegistryChainConfig
	if err := yaml.UnmarshalStrict(content, &chainConfig); err != nil {
		return
	}
	changed := false
	for i := range chainConfig.Chain.Steps {
		step := &chainConfig.Chain.Steps[i]
		if step.LiteralTestStep != nil {
			stale, c := filterStaleCredentials(&step.LiteralTestStep.Credentials, knownSecrets, bundleNames, filePath)
			if c {
				*allStale = append(*allStale, stale...)
				changed = true
			}
		}
	}
	if changed && !dryRun {
		writeRegistryFile(&chainConfig, filePath)
	}
}

func removeStaleFromWorkflowFile(filePath string, knownSecrets map[string]bool, bundleNames sets.Set[string], allStale *[]StaleCredential, dryRun bool) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return
	}
	var workflowConfig api.RegistryWorkflowConfig
	if err := yaml.UnmarshalStrict(content, &workflowConfig); err != nil {
		return
	}
	stale, changed := removeStaleFromMultiStage(&workflowConfig.Workflow.Steps, knownSecrets, bundleNames, filePath)
	if !changed {
		return
	}
	*allStale = append(*allStale, stale...)
	if !dryRun {
		writeRegistryFile(&workflowConfig, filePath)
	}
}

func countStaleFiles(stale []StaleCredential) int {
	files := make(map[string]bool)
	for _, s := range stale {
		files[s.FilePath] = true
	}
	return len(files)
}

// countUniqueFiles returns the number of unique file paths in a list of updates
func countUniqueFiles(updates []CredentialUpdate) int {
	files := make(map[string]bool)
	for _, update := range updates {
		files[update.FilePath] = true
	}
	return len(files)
}

// commentOutLine inserts "# " after the leading whitespace, preserving indentation.
// "      - mount_path: /foo" -> "      # - mount_path: /foo"
func commentOutLine(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(trimmed)]
	return indent + "# " + trimmed
}

// FindUnmigratedCredentials scans all configs and step-registry for old-style
// credential entries (namespace/name, no collection/bundle) that were not migrated.
func FindUnmigratedCredentials(releaseRepoPath string) ([]UnmigratedCredential, error) {
	var result []UnmigratedCredential

	// Scan ci-operator configs
	configDir := path.Join(releaseRepoPath, "ci-operator", "config")
	configCallback := func(cfg *api.ReleaseBuildConfiguration, info *config.Info) error {
		for i := range cfg.Tests {
			test := &cfg.Tests[i]
			if test.MultiStageTestConfiguration != nil {
				result = append(result, findUnmigratedInMultiStage(test.MultiStageTestConfiguration, info.Filename)...)
			}
			if test.MultiStageTestConfigurationLiteral != nil {
				result = append(result, findUnmigratedInMultiStageLiteral(test.MultiStageTestConfigurationLiteral, info.Filename)...)
			}
		}
		return nil
	}
	if err := config.OperateOnCIOperatorConfigDir(configDir, configCallback); err != nil {
		return nil, err
	}

	// Scan step-registry
	registryPath := path.Join(releaseRepoPath, "ci-operator", "step-registry")
	if err := filepath.WalkDir(registryPath, func(filePath string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "OWNERS" || strings.HasPrefix(d.Name(), "..") {
			return nil
		}
		switch {
		case strings.HasSuffix(filePath, "-ref.yaml"):
			content, err := os.ReadFile(filePath)
			if err != nil {
				return nil
			}
			var c api.RegistryReferenceConfig
			if err := yaml.UnmarshalStrict(content, &c); err != nil {
				return nil
			}
			result = append(result, findUnmigratedInCreds(c.Reference.Credentials, filePath)...)
		case strings.HasSuffix(filePath, "-chain.yaml"):
			content, err := os.ReadFile(filePath)
			if err != nil {
				return nil
			}
			var c api.RegistryChainConfig
			if err := yaml.UnmarshalStrict(content, &c); err != nil {
				return nil
			}
			for _, step := range c.Chain.Steps {
				if step.LiteralTestStep != nil {
					result = append(result, findUnmigratedInCreds(step.LiteralTestStep.Credentials, filePath)...)
				}
			}
		case strings.HasSuffix(filePath, "-workflow.yaml"):
			content, err := os.ReadFile(filePath)
			if err != nil {
				return nil
			}
			var c api.RegistryWorkflowConfig
			if err := yaml.UnmarshalStrict(content, &c); err != nil {
				return nil
			}
			result = append(result, findUnmigratedInMultiStage(&c.Workflow.Steps, filePath)...)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return result, nil
}

func findUnmigratedInMultiStage(ms *api.MultiStageTestConfiguration, filePath string) []UnmigratedCredential {
	var result []UnmigratedCredential
	for _, step := range ms.Pre {
		if step.LiteralTestStep != nil {
			result = append(result, findUnmigratedInCreds(step.LiteralTestStep.Credentials, filePath)...)
		}
	}
	for _, step := range ms.Test {
		if step.LiteralTestStep != nil {
			result = append(result, findUnmigratedInCreds(step.LiteralTestStep.Credentials, filePath)...)
		}
	}
	for _, step := range ms.Post {
		if step.LiteralTestStep != nil {
			result = append(result, findUnmigratedInCreds(step.LiteralTestStep.Credentials, filePath)...)
		}
	}
	return result
}

func findUnmigratedInMultiStageLiteral(ms *api.MultiStageTestConfigurationLiteral, filePath string) []UnmigratedCredential {
	var result []UnmigratedCredential
	for _, step := range ms.Pre {
		result = append(result, findUnmigratedInCreds(step.Credentials, filePath)...)
	}
	for _, step := range ms.Test {
		result = append(result, findUnmigratedInCreds(step.Credentials, filePath)...)
	}
	for _, step := range ms.Post {
		result = append(result, findUnmigratedInCreds(step.Credentials, filePath)...)
	}
	return result
}

func findUnmigratedInCreds(creds []api.CredentialReference, filePath string) []UnmigratedCredential {
	var result []UnmigratedCredential
	for _, cred := range creds {
		if (cred.Collection != "" && cred.Group != "") || cred.Bundle != "" {
			continue
		}
		if cred.Name != "" && cred.Namespace != "" {
			result = append(result, UnmigratedCredential{
				FilePath:  filePath,
				Name:      cred.Name,
				Namespace: cred.Namespace,
				MountPath: cred.MountPath,
			})
		}
	}
	return result
}

// CommentOutUnmigrated comments out unmigrated credential entries in their source files
// using raw text manipulation (no YAML re-serialization).
func CommentOutUnmigrated(entries []UnmigratedCredential, dryRun bool) (int, error) {
	// Group by file
	byFile := make(map[string][]UnmigratedCredential)
	for _, e := range entries {
		byFile[e.FilePath] = append(byFile[e.FilePath], e)
	}

	commented := 0
	for filePath, fileEntries := range byFile {
		content, err := os.ReadFile(filePath)
		if err != nil {
			logrus.WithError(err).Warnf("Failed to read %s", filePath)
			continue
		}
		lines := strings.Split(string(content), "\n")
		modified := false

		for _, entry := range fileEntries {
			// Find the line with "name: <entry.Name>" that is not already commented
			for i, line := range lines {
				trimmed := strings.TrimSpace(line)
				if trimmed == "name: "+entry.Name && !strings.HasPrefix(trimmed, "#") {
					// Verify context: mount_path should be on line above, namespace on line below
					if i > 0 && strings.Contains(lines[i-1], "mount_path:") && !strings.HasPrefix(strings.TrimSpace(lines[i-1]), "#") {
						if i+1 < len(lines) && strings.Contains(lines[i+1], "namespace:") && !strings.HasPrefix(strings.TrimSpace(lines[i+1]), "#") {
							// Get the indentation from the mount_path line
							indent := lines[i-1][:len(lines[i-1])-len(strings.TrimLeft(lines[i-1], " "))]

							// Insert explanation comment before mount_path
							commentLine := indent + "# this credential references a nonexistent Vault secret"
							lines = append(lines[:i-1], append([]string{commentLine}, lines[i-1:]...)...)
							// Adjust index for the inserted line
							i++

							lines[i-1] = commentOutLine(lines[i-1])
							lines[i] = commentOutLine(lines[i])
							lines[i+1] = commentOutLine(lines[i+1])

							// Check if the credentials: line above the comment should also be commented
							// After inserting comment line: lines[i-3]=credentials:, lines[i-2]=comment, lines[i-1]=mount_path
							if i >= 3 {
								credLine := strings.TrimSpace(lines[i-3])
								if credLine == "credentials:" && !strings.HasPrefix(credLine, "#") {
									// Check if there are other non-commented entries in this block
									hasOtherEntries := false
									for j := i + 2; j < len(lines); j++ {
										nextTrimmed := strings.TrimSpace(lines[j])
										if nextTrimmed == "" || (!strings.HasPrefix(nextTrimmed, "- ") && !strings.HasPrefix(nextTrimmed, "# ") && !strings.HasPrefix(nextTrimmed, "#- ")) {
											break
										}
										if strings.HasPrefix(nextTrimmed, "- mount_path:") {
											hasOtherEntries = true
											break
										}
									}
									if !hasOtherEntries {
										lines[i-3] = commentOutLine(lines[i-3])
									}
								}
							}

							modified = true
							commented++
							logrus.Infof("Commented out unmigrated credential: name=%s in %s", entry.Name, filePath)
							break
						}
					}
				}
			}
		}

		if modified && !dryRun {
			if err := os.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
				return commented, fmt.Errorf("failed to write %s: %w", filePath, err)
			}
		}
	}

	logrus.Infof("Commented out %d unmigrated credential entries", commented)
	return commented, nil
}

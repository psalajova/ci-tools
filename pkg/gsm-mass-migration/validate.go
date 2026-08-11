package gsmassmigration

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/config"
	gsmsecrets "github.com/openshift/ci-tools/pkg/gsm-secrets"
	gsmvalidation "github.com/openshift/ci-tools/pkg/gsm-validation"
)

type gsmReference struct {
	Collection    string
	Group         string
	Field         string // empty = auto-discovery, check any field exists for group
	Source        string
	PreNormalized bool // true for bundle refs (already in GSM format), false for credential stanzas
}

// ValidateGSMReferences checks that all GSM secrets referenced by bundles,
// credential stanzas, and secrets stanzas in the release repo actually exist in GSM.
func ValidateGSMReferences(
	ctx context.Context,
	gsmClient gsmsecrets.SecretManagerClient,
	projectNumber string,
	releaseRepoPath string,
	skipClusterProfileValidation bool,
) error {
	// 1. Collect all references
	refs, bundleNames, err := collectAllReferences(releaseRepoPath)
	if err != nil {
		return fmt.Errorf("failed to collect references: %w", err)
	}

	// Deduplicate references (resolve() expands cluster_groups, creating many identical bundle entries)
	type refKey struct{ collection, group, field string }
	seen := make(map[refKey]gsmReference)
	for _, ref := range refs {
		key := refKey{ref.Collection, ref.Group, ref.Field}
		if _, exists := seen[key]; !exists {
			seen[key] = ref
		}
	}
	var dedupedRefs []gsmReference
	for _, ref := range seen {
		dedupedRefs = append(dedupedRefs, ref)
	}
	logrus.Infof("Collected %d unique GSM references (%d before dedup) from %d bundles and configs/step-registry",
		len(dedupedRefs), len(refs), bundleNames.Len())

	// 2. Fetch indexes (one API call per unique collection)
	collections := sets.New[string]()
	for _, ref := range dedupedRefs {
		collections.Insert(ref.Collection)
	}

	indexes := make(map[string]sets.Set[string])
	for collection := range collections {
		indexSecretName := gsmsecrets.GetIndexSecretName(collection)
		resourceName := fmt.Sprintf("%s/secrets/%s",
			gsmsecrets.GetProjectResourceIdNumber(projectNumber),
			indexSecretName,
		)
		payload, err := gsmsecrets.GetSecretPayload(ctx, gsmClient, resourceName)
		if err != nil {
			logrus.Warnf("Could not read index for collection %s: %v", collection, err)
			indexes[collection] = sets.New[string]()
			continue
		}
		entries := gsmsecrets.ParseIndexSecretContent(payload)
		indexes[collection] = sets.New(entries...)
		logrus.Debugf("Collection %s: %d index entries", collection, len(entries))
	}
	logrus.Infof("Fetched indexes for %d collections", len(indexes))

	// 3. Validate
	var missing []gsmReference
	passed := 0
	for _, ref := range dedupedRefs {
		index := indexes[ref.Collection]

		// Bundle refs are already in GSM-normalized format (--dot--, __ delimiters).
		// Credential stanza refs may have / in group names that need converting.
		group := ref.Group
		field := ref.Field
		if !ref.PreNormalized {
			group = gsmvalidation.NormalizeName(group)
			if field != "" {
				field = gsmvalidation.NormalizeName(field)
			}
		}
		normalizedGroup := strings.ReplaceAll(group, "/", gsmvalidation.CollectionSecretDelimiter)

		if field == "" {
			prefix := normalizedGroup + gsmvalidation.CollectionSecretDelimiter
			found := false
			for entry := range index {
				if strings.HasPrefix(entry, prefix) {
					found = true
					break
				}
			}
			if !found {
				missing = append(missing, ref)
			} else {
				passed++
			}
		} else {
			entry := normalizedGroup + gsmvalidation.CollectionSecretDelimiter + field
			if index.Has(entry) {
				passed++
			} else {
				missing = append(missing, ref)
			}
		}
	}

	// 4. Report GSM reference validation
	logrus.Infof("GSM reference validation: %d passed, %d missing", passed, len(missing))
	for _, ref := range missing {
		field := ref.Field
		if field == "" {
			field = "(any)"
		}
		logrus.Errorf("MISSING secret: collection=%s group=%s field=%s source=%s", ref.Collection, ref.Group, field, ref.Source)
	}

	// 5. Validate cluster profile bundle coverage
	var profilesMissing []string
	if skipClusterProfileValidation {
		logrus.Info("Skipping cluster profile bundle validation (--skip-cluster-profile-validation=true)")
	} else {
		profilesMissing, err = validateClusterProfileBundles(releaseRepoPath, bundleNames)
		if err != nil {
			return fmt.Errorf("failed to validate cluster profile bundles: %w", err)
		}
	}

	totalErrors := len(missing) + len(profilesMissing)
	if totalErrors > 0 {
		return fmt.Errorf("validation failed: %d GSM secrets missing, %d cluster profiles without bundles", len(missing), len(profilesMissing))
	}

	logrus.Info("All validations passed")
	return nil
}

func collectAllReferences(releaseRepoPath string) ([]gsmReference, sets.Set[string], error) {
	var refs []gsmReference

	// Load bundles from gsm-config.yaml
	gsmConfigPath := path.Join(releaseRepoPath, "core-services/ci-secret-bootstrap/gsm-config.yaml")
	var gsmConfig api.GSMConfig
	if err := api.LoadGSMConfigFromFile(gsmConfigPath, &gsmConfig); err != nil {
		return nil, nil, fmt.Errorf("failed to load gsm-config.yaml: %w", err)
	}

	dptpCollection := gsmConfig.DPTPCollection
	if dptpCollection == "" {
		logrus.Infof("No DPTP collection defined in gsm-config.yaml, defaulting to %s", api.DPTPGSMCollection)
		dptpCollection = api.DPTPGSMCollection
	}

	bundleNames := sets.New[string]()
	for _, bundle := range gsmConfig.Bundles {
		bundleNames.Insert(bundle.Name)
		source := fmt.Sprintf("gsm-config.yaml (bundle: %s)", bundle.Name)

		// gsm_secrets (names are already in GSM-normalized format)
		for _, gsmSecret := range bundle.GSMSecrets {
			if len(gsmSecret.Fields) == 0 {
				refs = append(refs, gsmReference{
					Collection:    gsmSecret.Collection,
					Group:         gsmSecret.Group,
					Source:        source,
					PreNormalized: true,
				})
			} else {
				for _, field := range gsmSecret.Fields {
					refs = append(refs, gsmReference{
						Collection:    gsmSecret.Collection,
						Group:         gsmSecret.Group,
						Field:         field.Name,
						Source:        source,
						PreNormalized: true,
					})
				}
			}
		}

		// dockerconfig registries (names are already in GSM-normalized format)
		if bundle.DockerConfig != nil {
			for _, reg := range bundle.DockerConfig.Registries {
				refs = append(refs, gsmReference{
					Collection:    dptpCollection,
					Group:         reg.Group,
					Field:         reg.AuthField,
					Source:        source,
					PreNormalized: true,
				})
				if reg.EmailField != "" {
					refs = append(refs, gsmReference{
						Collection:    dptpCollection,
						Group:         reg.Group,
						Field:         reg.EmailField,
						Source:        source,
						PreNormalized: true,
					})
				}
			}
		}
	}

	// Collect credential and secrets stanza references from ci-operator configs
	configRefs, err := collectConfigRefs(releaseRepoPath, bundleNames)
	if err != nil {
		return nil, nil, err
	}
	refs = append(refs, configRefs...)

	// Collect credential stanza references from step-registry
	registryRefs, err := collectStepRegistryRefs(releaseRepoPath, bundleNames)
	if err != nil {
		return nil, nil, err
	}
	refs = append(refs, registryRefs...)

	return refs, bundleNames, nil
}

func collectConfigRefs(releaseRepoPath string, bundleNames sets.Set[string]) ([]gsmReference, error) {
	configDir := path.Join(releaseRepoPath, "ci-operator", "config")
	var refs []gsmReference

	callback := func(cfg *api.ReleaseBuildConfiguration, info *config.Info) error {
		for i := range cfg.Tests {
			test := &cfg.Tests[i]
			if test.MultiStageTestConfiguration != nil {
				refs = append(refs, extractMultiStageRefs(test.MultiStageTestConfiguration, bundleNames, info.Filename)...)
			}
			if test.MultiStageTestConfigurationLiteral != nil {
				refs = append(refs, extractMultiStageLiteralRefs(test.MultiStageTestConfigurationLiteral, bundleNames, info.Filename)...)
			}
			refs = append(refs, extractSecretRefs(bundleNames, info.Filename, test.Secret)...)
			refs = append(refs, extractSecretRefs(bundleNames, info.Filename, test.Secrets...)...)
		}
		return nil
	}

	if err := config.OperateOnCIOperatorConfigDir(configDir, callback); err != nil {
		return nil, err
	}
	return refs, nil
}

func collectStepRegistryRefs(releaseRepoPath string, bundleNames sets.Set[string]) ([]gsmReference, error) {
	registryPath := path.Join(releaseRepoPath, "ci-operator", "step-registry")
	var refs []gsmReference

	err := filepath.WalkDir(registryPath, func(filePath string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Name() == "OWNERS" || strings.HasPrefix(d.Name(), "..") {
			return nil
		}

		switch {
		case strings.HasSuffix(filePath, "-ref.yaml"):
			content, err := readFileBytes(filePath)
			if err != nil {
				return nil
			}
			var c api.RegistryReferenceConfig
			if err := yaml.UnmarshalStrict(content, &c); err != nil {
				return nil
			}
			refs = append(refs, extractCredentialRefs(c.Reference.Credentials, bundleNames, filePath)...)

		case strings.HasSuffix(filePath, "-chain.yaml"):
			content, err := readFileBytes(filePath)
			if err != nil {
				return nil
			}
			var c api.RegistryChainConfig
			if err := yaml.UnmarshalStrict(content, &c); err != nil {
				return nil
			}
			for _, step := range c.Chain.Steps {
				if step.LiteralTestStep != nil {
					refs = append(refs, extractCredentialRefs(step.LiteralTestStep.Credentials, bundleNames, filePath)...)
				}
			}

		case strings.HasSuffix(filePath, "-workflow.yaml"):
			content, err := readFileBytes(filePath)
			if err != nil {
				return nil
			}
			var c api.RegistryWorkflowConfig
			if err := yaml.UnmarshalStrict(content, &c); err != nil {
				return nil
			}
			refs = append(refs, extractMultiStageRefs(&c.Workflow.Steps, bundleNames, filePath)...)
		}
		return nil
	})

	return refs, err
}

func readFileBytes(filePath string) ([]byte, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		logrus.WithError(err).Warnf("Failed to read %s", filePath)
	}
	return content, err
}

func extractMultiStageRefs(ms *api.MultiStageTestConfiguration, bundleNames sets.Set[string], source string) []gsmReference {
	var refs []gsmReference
	for _, step := range ms.Pre {
		if step.LiteralTestStep != nil {
			refs = append(refs, extractCredentialRefs(step.LiteralTestStep.Credentials, bundleNames, source)...)
		}
	}
	for _, step := range ms.Test {
		if step.LiteralTestStep != nil {
			refs = append(refs, extractCredentialRefs(step.LiteralTestStep.Credentials, bundleNames, source)...)
		}
	}
	for _, step := range ms.Post {
		if step.LiteralTestStep != nil {
			refs = append(refs, extractCredentialRefs(step.LiteralTestStep.Credentials, bundleNames, source)...)
		}
	}
	return refs
}

func extractMultiStageLiteralRefs(ms *api.MultiStageTestConfigurationLiteral, bundleNames sets.Set[string], source string) []gsmReference {
	var refs []gsmReference
	for _, step := range ms.Pre {
		refs = append(refs, extractCredentialRefs(step.Credentials, bundleNames, source)...)
	}
	for _, step := range ms.Test {
		refs = append(refs, extractCredentialRefs(step.Credentials, bundleNames, source)...)
	}
	for _, step := range ms.Post {
		refs = append(refs, extractCredentialRefs(step.Credentials, bundleNames, source)...)
	}
	return refs
}

func extractCredentialRefs(credentials []api.CredentialReference, bundleNames sets.Set[string], source string) []gsmReference {
	var refs []gsmReference
	for _, cred := range credentials {
		if cred.Bundle != "" {
			if !bundleNames.Has(cred.Bundle) {
				logrus.Warnf("Credential references unknown bundle %q in %s", cred.Bundle, source)
			}
			continue
		}
		if cred.Collection == "" || cred.Group == "" {
			continue
		}
		refs = append(refs, gsmReference{
			Collection: cred.Collection,
			Group:      cred.Group,
			Field:      cred.Field,
			Source:     source,
		})
	}
	return refs
}

func extractSecretRefs(bundleNames sets.Set[string], source string, secrets ...*api.Secret) []gsmReference {
	var refs []gsmReference
	for _, s := range secrets {
		if s == nil {
			continue
		}
		if s.Bundle != "" {
			if !bundleNames.Has(s.Bundle) {
				logrus.Warnf("Secret references unknown bundle %q in %s", s.Bundle, source)
			}
			continue
		}
		if s.Collection == "" || s.Group == "" {
			continue
		}
		refs = append(refs, gsmReference{
			Collection: s.Collection,
			Group:      s.Group,
			Field:      s.Field,
			Source:     source,
		})
	}
	return refs
}

// validateClusterProfileBundles checks that every registered cluster profile
// has a corresponding bundle in gsm-config.yaml. Profiles that share secrets
// with another profile (via cluster-profiles-config.yaml `secret:` override)
// are expected to use the other profile's bundle and are skipped.
// Profiles that are known to not need bundles:
// - openshift-org-*: part of the "cluster profile sets" initiative, not yet active
var profilesToSkip = sets.New(
	"openshift-org-aws",
	"openshift-org-azure",
	"openshift-org-gcp",
)

func validateClusterProfileBundles(releaseRepoPath string, bundleNames sets.Set[string]) ([]string, error) {
	configPath := path.Join(releaseRepoPath, "ci-operator/step-registry/cluster-profiles/cluster-profiles-config.yaml")
	content, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read cluster-profiles-config.yaml: %w", err)
	}
	var profilesConfig api.ClusterProfiles
	if err := yaml.Unmarshal(content, &profilesConfig); err != nil {
		return nil, fmt.Errorf("failed to parse cluster-profiles-config.yaml: %w", err)
	}

	var missing []string
	covered := 0

	for _, profile := range profilesConfig.Items {
		if profile.IsASet() {
			continue
		}
		if profilesToSkip.Has(profile.Name) {
			logrus.Debugf("Skipping known exception: %s", profile.Name)
			continue
		}

		secretName := profile.Secret
		if secretName == "" {
			secretName = "cluster-secrets-" + profile.Name
		}

		if bundleNames.Has(secretName) {
			covered++
		} else {
			logrus.Errorf("MISSING bundle for cluster profile: %s (expected bundle name: %s)", profile.Name, secretName)
			missing = append(missing, secretName)
		}
	}

	checked := covered + len(missing)
	skipped := len(profilesConfig.Items) - checked
	logrus.Infof("Cluster profile bundle coverage: %d/%d covered, %d missing, %d skipped", covered, checked, len(missing), skipped)
	return missing, nil
}

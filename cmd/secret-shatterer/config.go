package main

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/config"
	"github.com/openshift/ci-tools/pkg/load"
	"github.com/openshift/ci-tools/pkg/registry"
	"github.com/sirupsen/logrus"
	"sigs.k8s.io/yaml"
)

// credentialRef holds a credential name and namespace
type credentialRef struct {
	name      string
	namespace string
}

// addCredential adds a credential to the list if not already seen
func addCredential(credentials *[]credentialRef, seen map[string]bool, name, namespace string) {
	key := name + ":" + namespace
	if seen[key] {
		return
	}
	seen[key] = true
	*credentials = append(*credentials, credentialRef{
		name:      name,
		namespace: namespace,
	})
}

// loadStepRegistry loads the step registry (workflows, chains, refs) from the release repo
func (o *options) loadStepRegistry() (registry.Resolver, error) {
	registryPath := path.Join(o.releaseRepoPath, "ci-operator", "step-registry")

	references, chains, workflows, _, _, _, observers, err := load.Registry(registryPath, load.RegistryFlat)
	if err != nil {
		return nil, fmt.Errorf("failed to load step registry: %w", err)
	}

	return registry.NewResolver(references, chains, workflows, observers, api.ClusterProfiles{}), nil
}

// collectCredentialsFromOrgRepo collects all credential names and namespaces used by the specified org/repo
func (o *options) collectCredentialsFromOrgRepo() []credentialRef {
	var credentials []credentialRef
	seen := make(map[string]bool) // Track "name:namespace" to deduplicate

	// Load the step registry resolver to resolve workflows/chains/refs
	resolver, err := o.loadStepRegistry()
	if err != nil {
		logrus.WithError(err).Warn("Failed to load step registry, credentials from workflows/chains may be incomplete")
	}

	// Collect from config files
	o.collectCredentialsFromConfigs(&credentials, seen, resolver)

	// Collect from job files (they may have credentials not in configs)
	o.collectCredentialsFromJobs(&credentials, seen)

	return credentials
}

// collectCredentialsFromConfigs scans config files and collects credential names and namespaces
func (o *options) collectCredentialsFromConfigs(credentials *[]credentialRef, seen map[string]bool, resolver registry.Resolver) {
	subdir := o.org + "/" + o.repo
	configDir := path.Join(o.releaseRepoPath, "ci-operator", "config", subdir)

	callback := func(config *api.ReleaseBuildConfiguration, info *config.Info) error {
		logrus.Tracef("Collecting credentials from: %s", info.Filename)

		// Process all tests for credentials
		for _, test := range config.Tests {
			if test.MultiStageTestConfiguration != nil {
				o.collectCredentialsFromMultiStage(test.MultiStageTestConfiguration, credentials, seen, resolver)
			}
			if test.MultiStageTestConfigurationLiteral != nil {
				o.collectCredentialsFromMultiStageLiteral(test.MultiStageTestConfigurationLiteral, credentials, seen)
			}
			// Container test secrets (Secret and Secrets fields) reference K8s secrets
			// that must also be migrated so their stanzas get rewritten in Phase 4.
			collectSecretsFromTest(&test, credentials, seen)
		}
		return nil
	}

	if err := config.OperateOnCIOperatorConfigDir(configDir, callback); err != nil {
		logrus.WithError(err).Warnf("Failed to scan config directory %s", configDir)
	}
}

// collectSecretsFromTest collects container test secrets (the Secret and Secrets
// fields) referenced by a test. Unlike credentials, these have no namespace
// (Phase 4 rewrites them via a name-only lookup), so we register them with an
// empty namespace and rely on target-name matching during enumeration.
func collectSecretsFromTest(test *api.TestStepConfiguration, credentials *[]credentialRef, seen map[string]bool) {
	var secrets []*api.Secret
	if test.Secret != nil {
		secrets = append(secrets, test.Secret)
	}
	secrets = append(secrets, test.Secrets...)

	for _, secret := range secrets {
		if secret == nil || secret.Name == "" {
			continue
		}
		// Skip already-migrated secrets
		if (secret.Collection != "" && secret.Group != "") || secret.Bundle != "" {
			continue
		}
		addCredential(credentials, seen, secret.Name, "")
	}
}

// collectCredentialsFromJobs scans job files for credentials
func (o *options) collectCredentialsFromJobs(credentials *[]credentialRef, seen map[string]bool) {
	subdir := o.org + "/" + o.repo
	jobsDir := path.Join(o.releaseRepoPath, "ci-operator", "jobs", subdir)

	if err := filepath.WalkDir(jobsDir, func(filePath string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || (!strings.HasSuffix(filePath, ".yaml") && !strings.HasSuffix(filePath, ".yml")) {
			return nil
		}

		content, err := os.ReadFile(filePath)
		if err != nil {
			logrus.WithError(err).Warnf("Failed to read job file %s", filePath)
			return nil
		}

		// Simple recursive search for credentials in job YAML
		var jobConfig map[string]interface{}
		if err := yaml.Unmarshal(content, &jobConfig); err != nil {
			return nil
		}

		o.extractCredentialsFromJobConfig(jobConfig, credentials, seen)
		return nil
	}); err != nil {
		logrus.WithError(err).Warnf("Failed to scan jobs directory %s", jobsDir)
	}
}

// collectCredentialsFromMultiStage collects credentials from multi-stage test configuration
func (o *options) collectCredentialsFromMultiStage(multiStage *api.MultiStageTestConfiguration, credentials *[]credentialRef, seen map[string]bool, resolver registry.Resolver) {
	if resolver == nil {
		// Fallback to collecting only from literal steps if resolver is not available
		logrus.Trace("Resolver not available, only collecting from literal steps")
		o.collectCredentialsFromMultiStageWithoutResolver(multiStage, credentials, seen)
		return
	}

	// Use the resolver to expand the workflow/chains/refs into literal steps
	resolved, err := resolver.Resolve("credential-collection", *multiStage)
	if err != nil {
		logrus.WithError(err).Warn("Failed to resolve MultiStageTestConfiguration, falling back to literal-only collection")
		o.collectCredentialsFromMultiStageWithoutResolver(multiStage, credentials, seen)
		return
	}

	// Now collect credentials from the resolved literal configuration
	o.collectCredentialsFromMultiStageLiteral(&resolved, credentials, seen)
}

// collectCredentialsFromMultiStageWithoutResolver collects only literal credentials (fallback)
func (o *options) collectCredentialsFromMultiStageWithoutResolver(multiStage *api.MultiStageTestConfiguration, credentials *[]credentialRef, seen map[string]bool) {
	for _, step := range multiStage.Pre {
		if step.LiteralTestStep != nil {
			for _, cred := range step.LiteralTestStep.Credentials {
				// Skip already-migrated credentials
				if (cred.Collection != "" && cred.Group != "") || cred.Bundle != "" {
					continue
				}
				addCredential(credentials, seen, cred.Name, cred.Namespace)
			}
		}
	}
	for _, step := range multiStage.Test {
		if step.LiteralTestStep != nil {
			for _, cred := range step.LiteralTestStep.Credentials {
				// Skip already-migrated credentials
				if (cred.Collection != "" && cred.Group != "") || cred.Bundle != "" {
					continue
				}
				addCredential(credentials, seen, cred.Name, cred.Namespace)
			}
		}
	}
	for _, step := range multiStage.Post {
		if step.LiteralTestStep != nil {
			for _, cred := range step.LiteralTestStep.Credentials {
				// Skip already-migrated credentials
				if (cred.Collection != "" && cred.Group != "") || cred.Bundle != "" {
					continue
				}
				addCredential(credentials, seen, cred.Name, cred.Namespace)
			}
		}
	}
}

// collectCredentialsFromMultiStageLiteral collects credentials from literal multi-stage configuration
func (o *options) collectCredentialsFromMultiStageLiteral(multiStage *api.MultiStageTestConfigurationLiteral, credentials *[]credentialRef, seen map[string]bool) {
	for _, step := range multiStage.Pre {
		for _, cred := range step.Credentials {
			// Skip already-migrated credentials
			if (cred.Collection != "" && cred.Group != "") || cred.Bundle != "" {
				continue
			}
			addCredential(credentials, seen, cred.Name, cred.Namespace)
		}
	}
	for _, step := range multiStage.Test {
		for _, cred := range step.Credentials {
			// Skip already-migrated credentials
			if (cred.Collection != "" && cred.Group != "") || cred.Bundle != "" {
				continue
			}
			addCredential(credentials, seen, cred.Name, cred.Namespace)
		}
	}
	for _, step := range multiStage.Post {
		for _, cred := range step.Credentials {
			// Skip already-migrated credentials
			if (cred.Collection != "" && cred.Group != "") || cred.Bundle != "" {
				continue
			}
			addCredential(credentials, seen, cred.Name, cred.Namespace)
		}
	}
}

// extractCredentialsFromJobConfig recursively searches for credentials in job config
func (o *options) extractCredentialsFromJobConfig(config interface{}, credentials *[]credentialRef, seen map[string]bool) {
	switch v := config.(type) {
	case map[string]interface{}:
		if creds, exists := v["credentials"]; exists {
			if credList, ok := creds.([]interface{}); ok {
				for _, cred := range credList {
					if credMap, ok := cred.(map[string]interface{}); ok {
						// Skip already-migrated credentials (they have collection+group or bundle instead of name)
						if _, hasCollection := credMap["collection"]; hasCollection {
							continue
						}
						if _, hasBundle := credMap["bundle"]; hasBundle {
							continue
						}

						name := ""
						namespace := ""
						if n, exists := credMap["name"]; exists {
							if nameStr, ok := n.(string); ok {
								name = nameStr
							}
						}
						if ns, exists := credMap["namespace"]; exists {
							if nsStr, ok := ns.(string); ok {
								namespace = nsStr
							}
						}
						if name != "" {
							addCredential(credentials, seen, name, namespace)
						}
					}
				}
			}
		}
		for _, value := range v {
			o.extractCredentialsFromJobConfig(value, credentials, seen)
		}
	case []interface{}:
		for _, item := range v {
			o.extractCredentialsFromJobConfig(item, credentials, seen)
		}
	}
}

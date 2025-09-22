package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"github.com/sirupsen/logrus"

	gsmassmigration "github.com/openshift/ci-tools/pkg/gsm-mass-migration"
)

const (
	GCP_PROJECT_ID     = "openshift-ci-secrets"
	GCP_PROJECT_NUMBER = "384486694155"
)

type options struct {
	dryRun                     bool
	gsmProjectName             string
	gsmProjectNumber           string
	releaseRepoPath            string
	org                        string
	repo                       string
	ocpContext                 string
	ocpNamespace               string
	targetedMode               bool
	massMigration              bool
	migrateCredentialsOnly     bool
	migrateClusterProfilesOnly bool
	migrateAll                 bool
	removeStaleCredentials     bool
	normalizeStepRegistry      bool
	skipGSMCreation            bool
	skipCSIFlag                bool
	addCSIFlag                 bool
	updateRoverGroups          bool
	validate                   bool
	commentOutUnmigrated       bool
	vaultCollectionsFile       string
	vaultCacheFile             string
	logLevel                   string
}

func parseOptions() *options {
	o := &options{}
	flagSet := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	flagSet.BoolVar(&o.dryRun, "dry-run", true, "Dry run")
	flagSet.StringVar(&o.gsmProjectName, "gsm-project-name", GCP_PROJECT_ID, "GSM project name")
	flagSet.StringVar(&o.gsmProjectNumber, "gsm-project-number", GCP_PROJECT_NUMBER, "GSM project number")
	flagSet.StringVar(&o.releaseRepoPath, "release-repo", os.Getenv("RELEASE_REPO"), "Path to local release repo (defaults to $RELEASE_REPO)")
	flagSet.StringVar(&o.org, "org", "", "Organization (required for targeted mode)")
	flagSet.StringVar(&o.repo, "repo", "", "Repository (required for targeted mode)")
	flagSet.StringVar(&o.ocpContext, "ocp-context", "b01", "OCP context to use for verification")
	flagSet.StringVar(&o.ocpNamespace, "ocp-namespace", "ci", "OCP namespace for cluster profile secrets")
	flagSet.BoolVar(&o.targetedMode, "targeted-mode", false, "Only process specified org/repo instead of mass migration")
	flagSet.BoolVar(&o.massMigration, "mass-migration", true, "Migrate all Vault secrets and cluster profiles to GSM")
	flagSet.BoolVar(&o.migrateCredentialsOnly, "migrate-credentials-only", false, "Only migrate credentials (skip cluster profiles)")
	flagSet.BoolVar(&o.migrateClusterProfilesOnly, "migrate-cluster-profiles-only", false, "Only migrate cluster profiles (skip credentials)")
	flagSet.BoolVar(&o.removeStaleCredentials, "remove-stale-credentials", false, "Remove old-format credentials that don't exist in Vault (post-migration cleanup)")
	flagSet.BoolVar(&o.normalizeStepRegistry, "normalize-step-registry", false, "Normalize step-registry YAML files (reformat only, no content changes)")
	flagSet.BoolVar(&o.skipGSMCreation, "skip-gsm-creation", false, "Skip GSM secret creation, only update config files (uses Vault cache for credential mapping)")
	flagSet.BoolVar(&o.skipCSIFlag, "skip-csi-flag", false, "Skip adding EnableSecretsStoreCSIDriver flag during credential migration")
	flagSet.BoolVar(&o.addCSIFlag, "add-csi-flag", false, "Add EnableSecretsStoreCSIDriver flag to all ci-operator configs (standalone mode)")
	flagSet.BoolVar(&o.updateRoverGroups, "update-rover-groups", false, "Update sync-rover-groups/_config.yaml with collection-to-group mappings from vault-collections file")
	flagSet.BoolVar(&o.validate, "validate", false, "Validate that all GSM secrets referenced by bundles and credential stanzas exist in GSM")
	flagSet.BoolVar(&o.commentOutUnmigrated, "comment-out-unmigrated", false, "Find and comment out unmigrated credential entries (run after --migrate-credentials-only)")
	flagSet.StringVar(&o.vaultCollectionsFile, "vault-collections-file", "vault-collections-owners.yaml", "Path to vault-collections-owners.yaml with rover group assignments")
	flagSet.StringVar(&o.vaultCacheFile, "vault-cache-file", "", "Path to cache Vault data on disk (dev only, speeds up repeated runs)")
	flagSet.StringVar(&o.logLevel, "log-level", "info", "Log level (panic, fatal, error, warn, info, debug, trace)")

	if err := flagSet.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Fatal("could not parse args")
	}
	return o
}

func (o *options) Validate() error {
	if o.releaseRepoPath == "" {
		return fmt.Errorf("--release-repo is required")
	}
	if o.targetedMode {
		if o.org == "" {
			return fmt.Errorf("--org is required for targeted mode")
		}
		if o.repo == "" {
			return fmt.Errorf("--repo is required for targeted mode")
		}
	}

	// Ensure only one migration mode is set
	if o.migrateCredentialsOnly && o.migrateClusterProfilesOnly {
		return fmt.Errorf("cannot use both --migrate-credentials-only and --migrate-cluster-profiles-only")
	}

	if o.skipCSIFlag && !o.migrateCredentialsOnly && !o.migrateAll {
		return fmt.Errorf("--skip-csi-flag is only valid with credential migration")
	}

	// Default to migrateAll if neither flag set
	if !o.migrateCredentialsOnly && !o.migrateClusterProfilesOnly {
		o.migrateAll = true
	}

	return nil
}

func (o *options) setupLogger() {
	level, parseErr := logrus.ParseLevel(o.logLevel)
	if parseErr != nil {
		logrus.WithError(parseErr).Fatal("Failed to parse log level")
	}
	logrus.SetLevel(level)

	formatter := logrus.Formatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		ForceColors:     true,
		TimestampFormat: time.RFC1123Z,
	})
	logrus.SetFormatter(formatter)
}

func main() {
	o := parseOptions()
	if err := o.Validate(); err != nil {
		logrus.WithError(err).Fatal("Failed to validate options")
	}
	o.setupLogger()

	if o.normalizeStepRegistry {
		logrus.Info("Normalizing step-registry YAML files...")
		if _, err := gsmassmigration.NormalizeStepRegistry(o.releaseRepoPath, o.dryRun); err != nil {
			logrus.WithError(err).Fatal("Normalization failed")
		}
		logrus.Info("Step-registry normalization complete!")
		return
	}

	if o.addCSIFlag {
		logrus.Info("Adding EnableSecretsStoreCSIDriver flag to all ci-operator configs...")
		count, err := gsmassmigration.AddCSIFlag(o.releaseRepoPath, o.dryRun)
		if err != nil {
			logrus.WithError(err).Fatal("Failed to add CSI flag")
		}
		logrus.Infof("Added CSI flag to %d configs", count)
		return
	}

	if o.updateRoverGroups {
		logrus.Info("Updating rover groups config with collection mappings...")
		if err := gsmassmigration.UpdateRoverGroupsConfig(o.releaseRepoPath, o.vaultCollectionsFile, o.dryRun); err != nil {
			logrus.WithError(err).Fatal("Failed to update rover groups config")
		}
		return
	}

	if o.validate {
		logrus.Info("Validating GSM references in release repo...")
		ctx := context.Background()
		gsmClient, err := secretmanager.NewClient(ctx)
		if err != nil {
			logrus.WithError(err).Fatal("Failed to create GSM client")
		}
		defer gsmClient.Close()
		if err := gsmassmigration.ValidateGSMReferences(ctx, gsmClient, o.gsmProjectNumber, o.releaseRepoPath); err != nil {
			logrus.WithError(err).Fatal("Validation failed")
		}
		return
	}

	if o.commentOutUnmigrated {
		logrus.Info("Finding and commenting out unmigrated credential entries...")
		entries, err := gsmassmigration.FindUnmigratedCredentials(o.releaseRepoPath)
		if err != nil {
			logrus.WithError(err).Fatal("Failed to find unmigrated credentials")
		}
		logrus.Infof("Found %d unmigrated credential entries", len(entries))
		for _, e := range entries {
			logrus.Warnf("UNMIGRATED: name=%s namespace=%s mount_path=%s file=%s", e.Name, e.Namespace, e.MountPath, e.FilePath)
		}
		if len(entries) > 0 {
			count, err := gsmassmigration.CommentOutUnmigrated(entries, o.dryRun)
			if err != nil {
				logrus.WithError(err).Fatal("Failed to comment out unmigrated credentials")
			}
			logrus.Infof("Commented out %d entries", count)
		}
		return
	}

	if o.removeStaleCredentials {
		logrus.Info("Removing stale credentials...")
		cache, err := loadVaultCache(o)
		if err != nil {
			logrus.WithError(err).Fatal("Failed to load Vault cache (needed for stale credential check)")
		}
		if _, err := gsmassmigration.RemoveStaleCredentials(o.releaseRepoPath, cache, o.dryRun); err != nil {
			logrus.WithError(err).Fatal("Failed to remove stale credentials")
		}
		logrus.Info("Stale credential removal complete!")
		return
	}

	logrus.Info("Starting Vault to GSM mass migration...")

	if err := o.runMassMigration(); err != nil {
		logrus.WithError(err).Fatal("Migration failed")
	}

	logrus.Info("Mass migration complete!")
}

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"github.com/sirupsen/logrus"

	gsmvalidation "github.com/openshift/ci-tools/pkg/gsm-validation"
	vaultgsmmigration "github.com/openshift/ci-tools/pkg/vault-gsm-migration"
	"github.com/openshift/ci-tools/pkg/vaultclient"
)

const (
	defaultVaultAddr = "https://vault.ci.openshift.org"
)

type options struct {
	vaultPath        string
	collection       string
	gsmProjectNumber string
	vaultAddr        string
	dryRun           bool
	dptp             bool
	logLevel         string
}

func gatherOptions() (*options, error) {
	o := &options{}

	flag.StringVar(&o.vaultPath, "vault-path", "", "Full Vault path (e.g., kv/dptp/cloud.openshift.com-pull-secret) (required)")
	flag.StringVar(&o.collection, "collection", "", "GSM collection name (required)")
	flag.StringVar(&o.gsmProjectNumber, "gsm-project-number", "384486694155", "GCP project number for GSM")
	flag.StringVar(&o.vaultAddr, "vault-addr", defaultVaultAddr, "Vault server address")
	flag.BoolVar(&o.dryRun, "dry-run", true, "Show what would be migrated without creating secrets")
	flag.BoolVar(&o.dptp, "dptp", false, "Mark secrets as DPTP secrets with label jira-project:dptp")
	flag.StringVar(&o.logLevel, "log-level", "debug", "Log level (debug, info, warn, error)")

	flag.Parse()

	if o.vaultPath == "" {
		return nil, fmt.Errorf("--vault-path is required")
	}
	if o.collection == "" {
		return nil, fmt.Errorf("--collection is required")
	}

	if !gsmvalidation.ValidateCollectionName(o.collection) {
		return nil, fmt.Errorf("invalid collection name %q: must match regex %s", o.collection, gsmvalidation.CollectionRegex)
	}

	return o, nil
}

func setupLogging(level string) error {
	logLevel, err := logrus.ParseLevel(level)
	if err != nil {
		return fmt.Errorf("invalid log level %q: %w", level, err)
	}
	logrus.SetLevel(logLevel)
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})
	return nil
}

func getVaultToken() (string, error) {
	// Try reading from ~/.vault-token first
	homeDir, err := os.UserHomeDir()
	if err == nil {
		tokenPath := fmt.Sprintf("%s/.vault-token", homeDir)
		tokenBytes, err := os.ReadFile(tokenPath)
		if err == nil && len(tokenBytes) > 0 {
			token := strings.TrimSpace(string(tokenBytes))
			if token != "" {
				logrus.Debugf("Using Vault token from %s", tokenPath)
				return token, nil
			}
		}
	}

	// Try VAULT_TOKEN environment variable
	token := os.Getenv("VAULT_TOKEN")
	if token != "" {
		logrus.Debug("Using Vault token from VAULT_TOKEN environment variable")
		return token, nil
	}

	return "", fmt.Errorf("no Vault token found (tried ~/.vault-token and VAULT_TOKEN env var)")
}

func main() {
	opts, err := gatherOptions()
	if err != nil {
		logrus.WithError(err).Fatal("Failed to parse options")
	}

	if err := setupLogging(opts.logLevel); err != nil {
		logrus.WithError(err).Fatal("Failed to setup logging")
	}

	ctx := context.Background()

	// Setup Vault client
	logrus.Infof("Connecting to Vault at %s", opts.vaultAddr)
	vaultToken, err := getVaultToken()
	if err != nil {
		logrus.WithError(err).Fatal("Failed to get Vault token")
	}

	vaultClient, err := vaultclient.New(opts.vaultAddr, vaultToken)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to create Vault client")
	}

	// Setup GSM client
	logrus.Info("Connecting to Google Secret Manager")
	gsmClient, err := secretmanager.NewClient(ctx)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to create GSM client (check GCP credentials with 'gcloud auth application-default login')")
	}
	defer gsmClient.Close()

	logrus.Infof("Starting migration:")
	logrus.Infof("  Vault path: %s", opts.vaultPath)
	logrus.Infof("  GSM collection: %s", opts.collection)
	if opts.dryRun {
		logrus.Info("  Mode: DRY RUN (no secrets will be created)")
	}

	// Perform migration
	createdSecrets, err := vaultgsmmigration.MigrateVaultSecretToGSM(
		ctx,
		vaultClient,
		gsmClient,
		opts.vaultPath,
		opts.collection,
		"",
		opts.gsmProjectNumber,
		opts.dryRun,
		opts.dptp,
	)

	// Report results
	if err != nil {
		logrus.WithError(err).Errorf("Migration completed with errors")
		if len(createdSecrets) > 0 {
			logrus.Infof("Partially successful: %d secrets created/would be created:", len(createdSecrets))
			for _, secretName := range createdSecrets {
				logrus.Infof("  - %s", secretName)
			}
		}
		os.Exit(1)
	}

	logrus.Infof("Migration completed successfully!")
	logrus.Infof("Created/would create %d GSM secrets:", len(createdSecrets))
	for _, secretName := range createdSecrets {
		logrus.Infof("  - %s", secretName)
	}
}

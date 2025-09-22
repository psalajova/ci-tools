package vaultgsmmigration_test

import (
	"context"
	"fmt"
	"log"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"

	vaultgsmmigration "github.com/openshift/ci-tools/pkg/vault-gsm-migration"
	"github.com/openshift/ci-tools/pkg/vaultclient"
)

// Example_massMigration shows how to use the package functions for mass migration scripts.
func Example_massMigration() {
	ctx := context.Background()

	// Setup clients
	vaultClient, err := vaultclient.New("https://vault.ci.openshift.org", "your-token")
	if err != nil {
		log.Fatal(err)
	}

	gsmClient, err := secretmanager.NewClient(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer gsmClient.Close()

	// List of Vault paths to migrate
	vaultPaths := []string{
		"kv/dptp/cloud.openshift.com-pull-secret",
		"kv/dptp/github-token",
		"kv/dptp/aws-credentials",
	}

	collection := "test-platform-infra"
	projectNumber := "384486694155" // openshift-ci-secrets production project

	// Migrate each secret
	for _, vaultPath := range vaultPaths {
		fmt.Printf("Migrating %s...\n", vaultPath)

		createdSecrets, err := vaultgsmmigration.MigrateVaultSecretToGSM(
			ctx,
			vaultClient,
			gsmClient,
			vaultPath,
			collection,
			"",
			projectNumber,
			false, // dryRun
			true,  // dptp - mark as DPTP secrets
		)

		if err != nil {
			fmt.Printf("  Error: %v\n", err)
			continue
		}

		fmt.Printf("  Created %d secrets\n", len(createdSecrets))
	}
}

// Example_lowLevel shows how to use lower-level functions for custom migration logic.
func Example_lowLevel() {
	vaultPath := "kv/dptp/cloud.openshift.com-pull-secret"

	// Extract group name
	group, err := vaultgsmmigration.ExtractGroupFromVaultPath(vaultPath)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Group: %s\n", group)

	// Read fields from Vault (requires actual Vault client)
	// vaultClient, _ := vaultclient.New("https://vault.ci.openshift.org", "token")
	// fields, err := vaultgsmmigration.GetFieldsFromVault(vaultClient, vaultPath)
	// if err != nil {
	//     log.Fatal(err)
	// }
	//
	// // Process fields with custom logic
	// for fieldName, fieldValue := range fields {
	//     fmt.Printf("Field: %s = %s\n", fieldName, fieldValue)
	// }
}

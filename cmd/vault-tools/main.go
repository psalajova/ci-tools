package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/openshift/ci-tools/pkg/gsm-secrets"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: vault-tools <vault-secrets.json>")
		os.Exit(1)
	}

	filename := os.Args[1]

	secretData, err := processVaultSecrets(filename)
	if err != nil {
		log.Fatal(err)
	}

	//findTooLongStrings(secretData)
	printSecretPaths(secretData)
}

func findTooLongStrings(secretData *VaultSecretsData) {
	fmt.Printf("Secrets longer than 255:\n")
	for _, secret := range secretData.Secrets {
		collection, group := extractCollectionAndGroup(secret.Path)
		if collection == "" || group == "" {
			fmt.Printf("Invalid path '%s'\n", secret.Path)
			continue
		}

		for _, key := range secret.Keys {
			if isSelfServiceKey(key) {
				continue
			}
			gsmName := getGSMSecretName(collection, group, key)
			fmt.Printf("%s (%d)\n", gsmName, len(gsmName))
		}
	}
}

func printSecretPaths(secretData *VaultSecretsData) {
	for _, secret := range secretData.Secrets {
		if strings.Count(secret.Path, "/") != 1 {
			fmt.Println(secret.Path)
		}
	}
}

// extractCollectionAndGroup extracts collection and group from a path like "microshift/copr"
// Returns empty strings if the path doesn't match the expected pattern
func extractCollectionAndGroup(path string) (collection string, group string) {
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func isSelfServiceKey(secretName string) bool {
	return secretName == "secretsync/target-namespace" || secretName == "secretsync/target-name"
}

func getGSMSecretName(collection, group, secretName string) string {
	return gsmsecrets.GetGSMSecretName(collection, group, secretName)
}

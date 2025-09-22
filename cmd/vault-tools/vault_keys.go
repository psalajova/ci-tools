package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	GsmDelimiter = "__"
)

// VaultSecretsData represents the JSON structure from list-vault-secrets.sh
type VaultSecretsData struct {
	Secrets []VaultSecretInfo `json:"secrets"`
}

// VaultSecretInfo contains the path and keys for a single secret
type VaultSecretInfo struct {
	Path string   `json:"path"`
	Keys []string `json:"keys"`
}

//   {
//    "secrets": [
//      {
//        "path": "kv/selfservice/confidential-qe/aws-qe",
//        "keys": [".awscred", "secretsync/target-name", "secretsync/target-namespace",
//  "ssh-privatekey", "ssh-publickey"]
//      },
//      {
//        "path": "kv/selfservice/acm-qe/some-secret",
//        "keys": ["token", "username", "password"]
//      },
//      {
//        "path": "kv/selfservice/folder/nested/deeper/secret-name",
//        "keys": ["key1", "key2", "key3"]
//      }
//    ]
//  }

// loadVaultSecrets reads the vault-secrets.json file and parses it
func loadVaultSecrets(filename string) (*VaultSecretsData, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	var secrets VaultSecretsData
	if err := json.Unmarshal(data, &secrets); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	// Remove the "kv/selfservice/" prefix from all paths
	for i := range secrets.Secrets {
		secrets.Secrets[i].Path = strings.Replace(secrets.Secrets[i].Path, "kv/selfservice/", "", 1)
	}

	return &secrets, nil
}

// Example usage function
func processVaultSecrets(filename string) (*VaultSecretsData, error) {
	secrets, err := loadVaultSecrets(filename)
	if err != nil {
		return nil, err
	}

	fmt.Printf("Found %d secrets\n", len(secrets.Secrets))
	return secrets, nil
}

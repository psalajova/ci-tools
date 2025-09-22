package gsmassmigration

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/sirupsen/logrus"
)

// GetVaultPathFromOCPSecret queries OpenShift clusters to find the Vault source path for a secret.
// Tries the b01 cluster first (most common), then falls back to app.ci.
// Returns the vault path from the secretsync-vault-source-path annotation.
func GetVaultPathFromOCPSecret(secretName, namespace string) (string, error) {
	// Try b01 cluster first
	vaultPath, err := queryOCPSecret("b01", secretName, namespace)
	if err == nil && vaultPath != "" {
		logrus.Debugf("Found secret %s/%s on b01 cluster", namespace, secretName)
		return vaultPath, nil
	}

	// Fallback to app.ci cluster
	vaultPath, err = queryOCPSecret("app.ci", secretName, namespace)
	if err == nil && vaultPath != "" {
		logrus.Debugf("Found secret %s/%s on app.ci cluster", namespace, secretName)
		return vaultPath, nil
	}

	return "", fmt.Errorf("secret %s/%s not found on b01 or app.ci clusters", namespace, secretName)
}

// queryOCPSecret runs an oc command to get the vault source path from a secret annotation.
// Command: oc --context <ctx> get secret <name> -n <ns> -o jsonpath='{.data.secretsync-vault-source-path}' | base64 -d
func queryOCPSecret(context, secretName, namespace string) (string, error) {
	// Get the base64-encoded vault path
	cmd := exec.Command("oc", "--context", context, "get", "secret", secretName,
		"-n", namespace,
		"-o", "jsonpath={.data.secretsync-vault-source-path}")

	output, err := cmd.CombinedOutput()
	if err != nil {
		logrus.Debugf("Failed to query secret %s/%s on %s: %v", namespace, secretName, context, err)
		return "", err
	}

	encodedPath := strings.TrimSpace(string(output))
	if encodedPath == "" {
		return "", fmt.Errorf("secret %s/%s exists but has no secretsync-vault-source-path", namespace, secretName)
	}

	// Decode base64
	decodeCmd := exec.Command("base64", "-d")
	decodeCmd.Stdin = strings.NewReader(encodedPath)
	decodedOutput, err := decodeCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to decode vault path for %s/%s: %w", namespace, secretName, err)
	}

	vaultPath := strings.TrimSpace(string(decodedOutput))
	return vaultPath, nil
}

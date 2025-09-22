package gsmassmigration

import (
	"fmt"
	"strings"
)

const (
	SecretSyncTargetName      = "secretsync/target-name"
	SecretSyncTargetNamespace = "secretsync/target-namespace"
	SecretSyncTargetClusters  = "secretsync/target-clusters"
)

// ParseVaultPath extracts collection and group from a Vault path.
// Input: "kv/selfservice/vsphere/ibmcloud/config"
// Output: collection="vsphere", group="ibmcloud/config"
//
// Path format: kv/selfservice/collection/group/...
// Group preserves slashes (e.g., "ibmcloud/config" not "ibmcloud__config")
func ParseVaultPath(vaultPath string) (collection, group string, err error) {
	parts := strings.Split(vaultPath, "/")
	if len(parts) < 4 || parts[0] != "kv" || parts[1] != "selfservice" {
		return "", "", fmt.Errorf("invalid vault path format, expected kv/selfservice/<collection>/<group>/...: %s", vaultPath)
	}

	collection = parts[2]                // e.g., "vsphere"
	group = strings.Join(parts[3:], "/") // e.g., "ibmcloud/config" - preserve /

	return collection, group, nil
}

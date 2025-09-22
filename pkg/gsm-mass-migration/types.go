package gsmassmigration

// VaultSecretPath represents a Vault secret location and its GSM mapping
type VaultSecretPath struct {
	FullPath   string // e.g., "kv/selfservice/vsphere/ibmcloud/config"
	Collection string // e.g., "vsphere" (extracted from path)
	Group      string // e.g., "ibmcloud/config" (preserves /)
}

// MigrationResult tracks success/failure of a single secret migration
type MigrationResult struct {
	VaultPath        string
	Collection       string
	Group            string
	SecretName       string   // The K8s secret name (from secretsync/target-name)
	TargetNamespaces string   // Comma-separated K8s namespaces (from secretsync/target-namespace)
	CreatedFields    []string // GSM secret names created
	Error            error
}

// CredentialUpdate represents a credential stanza change in a config/step-registry file
type CredentialUpdate struct {
	FilePath      string // Path to the YAML file that was modified
	OldNamespace  string
	OldName       string
	NewCollection string // Set for vault secret migration
	NewGroup      string // Set for vault secret migration
	Bundle        string // Set for bundle conversion (mutually exclusive with NewCollection/NewGroup)
	IsSecret      bool   // True for container test secrets: entries (no namespace field)
	KeepNamespace bool   // True for sync_to_cluster bundles (namespace must be preserved)
}

// UnmigratedCredential represents an old-style credential entry that could not be migrated
type UnmigratedCredential struct {
	FilePath  string
	Name      string
	Namespace string
	MountPath string
}

// CachedVaultSecret holds full secret data read from Vault, including all fields
type CachedVaultSecret struct {
	Path            string            // Full Vault path (e.g., "kv/selfservice/vsphere/ibmcloud/config")
	Collection      string            // GSM collection (e.g., "vsphere")
	Group           string            // GSM group (e.g., "ibmcloud/config")
	TargetName      string            // K8s secret name from secretsync/target-name metadata
	TargetNamespace string            // K8s namespace from secretsync/target-namespace metadata
	TargetClusters  string            // Comma-separated cluster list from secretsync/target-clusters (empty = all user_secrets_target_clusters)
	IsEmpty         bool              // True if secret has no non-metadata fields
	IsPlaceholder   bool              // True if secret is a placeholder (.awscred, etc.)
	Fields          map[string]string // All non-metadata fields (field name → value)
}

// VaultCache holds cached Vault secrets with indexes for fast lookup
type VaultCache struct {
	Secrets map[string]*CachedVaultSecret // Key: vault path

	// Indexes for fast lookup
	ByTargetName map[string][]*CachedVaultSecret // For cluster profile bundle lookups
	ByCollection map[string][]*CachedVaultSecret // For filtering by collection
}

package vaultmigrationanalyzer

// VaultSecret represents a secret in Vault
type VaultSecret struct {
	Path           string
	Collection     string
	Group          string
	Fields         []string
	FieldCount     int
	IsEmpty        bool
	IsPlaceholder  bool
	SecretSyncMeta SecretSyncMetadata
}

// SecretSyncMetadata contains secretsync/* metadata from Vault
type SecretSyncMetadata struct {
	TargetName      string
	TargetNamespace string
	TargetCluster   string
}

// ValidationFailure represents a validation error
type ValidationFailure struct {
	Path             string
	Field            string
	OriginalName     string
	NormalizedName   string
	UnsupportedChars []rune
	Issue            string
	GSMSecretName    string
	GSMSecretLength  int
	MaxAllowedLength int
}

// ClusterProfileSecretDefinition represents a secret definition from _config.yaml
type ClusterProfileSecretDefinition struct {
	Name              string
	Items             ClusterProfileItems
	Fields            []FieldMapping
	Targets           []ClusterProfileTarget
	DockerConfigKey   string                 // The K8s secret key name (e.g., "pull-secret")
	DockerConfigItems []DockerConfigItemInfo // Full registry info for bundle generation
}

// ClusterProfileItems categorizes items by source
type ClusterProfileItems struct {
	DPTP        []string
	Selfservice []string
}

// FieldMapping represents a field extracted from an item
type FieldMapping struct {
	Field    string
	FromItem string
	Pattern  string // "simple_field", "dockerconfig", "nested", "exotic"
}

// ClusterProfileTarget represents where a cluster profile is deployed
type ClusterProfileTarget struct {
	ClusterGroups []string
	Namespace     string
}

// DockerConfigItemInfo stores full registry information for bundle generation
type DockerConfigItemInfo struct {
	Item        string
	RegistryURL string
	AuthField   string
	EmailField  string
}

// FieldMappingPattern represents a category of field mappings
type FieldMappingPattern struct {
	PatternType string
	Count       int
	Examples    []FieldMapping
}

// TargetPattern represents a common cluster_groups + namespace combination
type TargetPattern struct {
	ClusterGroups []string
	Namespace     string
	SecretCount   int
}

// DualPurposeSecret represents a secret used in both cluster profiles and multi-stage
type DualPurposeSecret struct {
	VaultPath             string
	UsedInClusterProfiles []string
	UsedInMultiStage      []string
}

// Report is the final output of the analyzer
type Report struct {
	VaultInventory        VaultInventoryReport
	ClusterProfiles       ClusterProfileReport
	MultiStageCredentials MultiStageCredReport
	FieldMappingPatterns  FieldMappingReport
	TargetDistribution    TargetDistributionReport
	GSMQuota              GSMQuotaReport
	CrossReference        CrossReferenceReport
	MigrationReadiness    MigrationReadinessReport
}

// VaultInventoryReport contains Phase 0-1 results
type VaultInventoryReport struct {
	TotalDPTPSecrets        int
	TotalSelfserviceSecrets int
	EmptyOrPlaceholder      []string
	ValidationFailures      ValidationFailuresReport
	NormalizationApplied    NormalizationStats
	UnexpectedPaths         []string
}

// ValidationFailuresReport contains all validation failures
type ValidationFailuresReport struct {
	UnsupportedCharsInNames  []ValidationFailure
	UnsupportedCharsInFields []ValidationFailure
	NameLengthExceeded       []ValidationFailure
}

// NormalizationStats tracks how many normalizations were applied
type NormalizationStats struct {
	DotsConverted        int
	UnderscoresConverted int
}

// ClusterProfileReport contains Phase 2 results
type ClusterProfileReport struct {
	TotalDefinitions int
	Definitions      []ClusterProfileSecretDefinition
	ItemUsageMap     map[string]ItemUsage
}

// ItemUsage tracks which cluster profiles use an item
type ItemUsage struct {
	UsedByProfiles []string
	UsageCount     int
}

// MultiStageCredReport contains Phase 3 results
type MultiStageCredReport struct {
	TotalUniqueCredentials int
	CredentialUsage        map[string]CredentialUsage
}

// CredentialUsage tracks which orgs/repos use a credential
type CredentialUsage struct {
	UsedBy     []string // "org/repo" format
	UsageCount int
}

// FieldMappingReport contains Phase 4 results
type FieldMappingReport struct {
	Patterns map[string]FieldMappingPattern
}

// TargetDistributionReport contains Phase 5 results
type TargetDistributionReport struct {
	UniqueClusterGroups []string
	UniqueNamespaces    []string
	CommonPatterns      []TargetPattern
}

// GSMQuotaReport contains Phase 6 results
type GSMQuotaReport struct {
	TotalSecretsToCreate int
	EstimatedVersions    int
	CurrentQuota         QuotaInfo
	MigrationFeasible    bool
	QuotaIncreaseNeeded  bool
	RecommendedQuota     int
}

// QuotaInfo contains current GSM quota information
type QuotaInfo struct {
	MaxSecrets   int
	CurrentUsage int
	Available    int
}

// CrossReferenceReport contains Phase 7 results
type CrossReferenceReport struct {
	DualPurposeSecrets           []DualPurposeSecret
	SecretSyncMetadataMismatches []SecretSyncMismatch
	OrphanedSecrets              []string
	MissingSecrets               []MissingSecret
}

// SecretSyncMismatch represents a mismatch between metadata and actual usage
type SecretSyncMismatch struct {
	Secret             string
	MetadataTargetName string
	ActualUsage        string
	Issue              string
}

// MissingSecret represents a referenced but non-existent secret
type MissingSecret struct {
	ReferencedIn string
	ItemName     string
	Issue        string
}

// MigrationReadinessReport summarizes readiness
type MigrationReadinessReport struct {
	TotalSecretsToMigrate     int
	TotalGSMSecretsToCreate   int
	CriticalBlockers          BlockersReport
	FieldMappingCoverage      CoverageReport
	DeduplicationIntelligence DeduplicationReport
}

// BlockersReport lists all blockers
type BlockersReport struct {
	UnexpectedVaultPaths int
	ValidationFailures   int
	NameLengthViolations int
	MissingSecrets       int
	QuotaInsufficient    bool
}

// CoverageReport tracks field mapping understanding
type CoverageReport struct {
	AllPatternsUnderstood       bool
	ExoticPatternsNeedingReview []string
}

// DeduplicationReport provides deduplication intelligence
type DeduplicationReport struct {
	SecretsUsedMultipleTimes map[string]MultiUseSecret
}

// MultiUseSecret tracks a secret used multiple times
type MultiUseSecret struct {
	UsageCount int
	UsedBy     []string
}

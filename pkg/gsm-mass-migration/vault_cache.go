package gsmassmigration

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/openshift/ci-tools/pkg/vaultclient"
)

// BuildVaultCache scans kv/dptp and kv/selfservice, reads full field data,
// and builds an in-memory cache with indexes for fast lookup.
func BuildVaultCache(vaultClient *vaultclient.VaultClient) (*VaultCache, error) {
	cache := &VaultCache{
		Secrets:      make(map[string]*CachedVaultSecret),
		ByTargetName: make(map[string][]*CachedVaultSecret),
		ByCollection: make(map[string][]*CachedVaultSecret),
	}

	// Scan kv/dptp
	logrus.Info("Scanning kv/dptp...")
	if err := scanAndCacheBasePath(vaultClient, "kv/dptp", cache); err != nil {
		return nil, fmt.Errorf("failed to scan kv/dptp: %w", err)
	}

	// Scan kv/selfservice
	logrus.Info("Scanning kv/selfservice...")
	if err := scanAndCacheBasePath(vaultClient, "kv/selfservice", cache); err != nil {
		return nil, fmt.Errorf("failed to scan kv/selfservice: %w", err)
	}

	logrus.Infof("Vault cache built: %d secrets cached", len(cache.Secrets))
	return cache, nil
}

// scanAndCacheBasePath scans a Vault base path and populates the cache
func scanAndCacheBasePath(vaultClient *vaultclient.VaultClient, basePath string, cache *VaultCache) error {
	logrus.Debugf("Attempting to list Vault path: %s", basePath)
	allPaths, err := vaultClient.ListKVRecursively(basePath)
	if err != nil {
		logrus.WithError(err).Errorf("ListKVRecursively failed for %s", basePath)
		return fmt.Errorf("failed to list %s: %w", basePath, err)
	}

	logrus.Debugf("Found %d paths under %s", len(allPaths), basePath)

	for _, path := range allPaths {
		// Parse collection and group
		collection, group, err := ParseVaultPath(path)
		if err != nil {
			// DPTP secrets don't follow selfservice format
			if strings.HasPrefix(path, "kv/dptp/") {
				parts := strings.Split(path, "/")
				if len(parts) >= 3 {
					collection = "test-platform-infra"
					// Extract group name exactly as it appears in Vault
					group = strings.Join(parts[2:], "/")
				} else {
					logrus.WithError(err).Warnf("Skipping invalid DPTP path: %s", path)
					continue
				}
			} else {
				logrus.WithError(err).Warnf("Skipping invalid path: %s", path)
				continue
			}
		}

		// Read full secret data from Vault
		kvData, err := vaultClient.GetKV(path)
		if err != nil {
			logrus.WithError(err).Warnf("Failed to read %s, skipping", path)
			continue
		}

		if kvData.Data == nil {
			logrus.Warnf("No data in %s, skipping", path)
			continue
		}

		// Extract all non-metadata fields
		fields := make(map[string]string)
		for key, value := range kvData.Data {
			// Skip metadata fields (secretsync/* fields are for OCP sync, not GSM fields)
			if strings.HasPrefix(key, "secretsync/") {
				continue
			}
			fields[key] = value
		}

		// Check if empty or placeholder
		isEmpty := len(fields) == 0
		isPlaceholder := false
		if len(fields) == 1 {
			for key := range fields {
				if key == "placeholder" {
					isPlaceholder = true
					break
				}
			}
		}

		// Extract secretsync metadata (only for selfservice secrets)
		targetName := ""
		targetNamespace := ""
		if collection != "test-platform-infra" {
			if val, exists := kvData.Data[SecretSyncTargetName]; exists {
				targetName = val
			}
			if val, exists := kvData.Data[SecretSyncTargetNamespace]; exists {
				targetNamespace = val
			}
		}
		targetClusters := ""
		if val, exists := kvData.Data[SecretSyncTargetClusters]; exists {
			targetClusters = val
		}

		// Create cached secret
		cached := &CachedVaultSecret{
			Path:            path,
			Collection:      collection,
			Group:           group,
			TargetName:      targetName,
			TargetNamespace: targetNamespace,
			TargetClusters:  targetClusters,
			IsEmpty:         isEmpty,
			IsPlaceholder:   isPlaceholder,
			Fields:          fields,
		}

		// Add to main map
		cache.Secrets[path] = cached

		// Build indexes
		if targetName != "" {
			cache.ByTargetName[targetName] = append(cache.ByTargetName[targetName], cached)
		}
		cache.ByCollection[collection] = append(cache.ByCollection[collection], cached)

		//logrus.Debugf("Cached secret: %s (collection=%s, group=%s, fields=%d)", path, collection, group, len(fields))
	}

	logrus.Debugf("Cached %d secrets from %s", len(cache.Secrets), basePath)
	return nil
}

// GetByPath retrieves a cached secret by its Vault path
func (c *VaultCache) GetByPath(path string) *CachedVaultSecret {
	return c.Secrets[path]
}

// GetByTargetName retrieves all cached secrets with the given target name
func (c *VaultCache) GetByTargetName(name string) []*CachedVaultSecret {
	return c.ByTargetName[name]
}

// GetByCollection retrieves all cached secrets in the given collection
func (c *VaultCache) GetByCollection(collection string) []*CachedVaultSecret {
	return c.ByCollection[collection]
}

// FilterDPTPByItems returns DPTP secrets whose group (item name) is in the given set
func (c *VaultCache) FilterDPTPByItems(items map[string]bool) []*CachedVaultSecret {
	var filtered []*CachedVaultSecret

	dptpSecrets := c.GetByCollection("test-platform-infra")
	logrus.Debugf("Filtering %d DPTP secrets from cache against %d requested items", len(dptpSecrets), len(items))

	// Strip dptp/ prefix from items if present (added by secretbootstrap.resolve())
	normalizedItems := make(map[string]bool)
	for item := range items {
		normalized := strings.TrimPrefix(item, "dptp/")
		normalizedItems[normalized] = true
	}

	for _, secret := range dptpSecrets {
		// Skip empty/placeholder secrets
		if secret.IsEmpty || secret.IsPlaceholder {
			continue
		}

		// Check using normalized item names (without dptp/ prefix)
		if normalizedItems[secret.Group] {
			filtered = append(filtered, secret)
			logrus.Debugf("Matched DPTP secret: %s (group=%s)", secret.Path, secret.Group)
		}
	}

	logrus.Debugf("Filtered DPTP secrets: %d matched out of %d", len(filtered), len(dptpSecrets))
	return filtered
}

// SaveToFile serializes the cache to a JSON file for reuse across runs (dev only).
func (c *VaultCache) SaveToFile(path string) error {
	data, err := json.Marshal(c.Secrets)
	if err != nil {
		return fmt.Errorf("failed to marshal cache: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write cache file: %w", err)
	}
	logrus.Infof("Vault cache saved to %s (%d secrets)", path, len(c.Secrets))
	return nil
}

// LoadFromFile deserializes a cache from a JSON file and rebuilds indexes.
func LoadFromFile(path string) (*VaultCache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var secrets map[string]*CachedVaultSecret
	if err := json.Unmarshal(data, &secrets); err != nil {
		return nil, fmt.Errorf("failed to parse cache file: %w", err)
	}
	cache := &VaultCache{
		Secrets:      secrets,
		ByTargetName: make(map[string][]*CachedVaultSecret),
		ByCollection: make(map[string][]*CachedVaultSecret),
	}
	for _, cached := range secrets {
		if cached.TargetName != "" {
			cache.ByTargetName[cached.TargetName] = append(cache.ByTargetName[cached.TargetName], cached)
		}
		cache.ByCollection[cached.Collection] = append(cache.ByCollection[cached.Collection], cached)
	}
	logrus.Infof("Vault cache loaded from %s (%d secrets)", path, len(secrets))
	return cache, nil
}

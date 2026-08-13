package gsmassmigration

import (
	"sort"

	"github.com/sirupsen/logrus"
)

// MigrationReport summarizes the results of a migration run
type MigrationReport struct {
	TotalSecrets                   int
	SuccessfulSecrets              int
	FailedSecrets                  int
	TotalFields                    int
	CredentialUpdates              int
	BundlesAddedToGSMConfig        int
	ConfigEntriesRemovedFromConfig int
	Errors                         []error
	NotMigratedSecrets             []string
	IndexUpdateFailures            []string
}

// GenerateReport creates a summary report from migration results and credential updates.
// If cache is provided, it also computes which vault secrets were not migrated.
func GenerateReport(migrations []MigrationResult, credUpdates []CredentialUpdate, cache *VaultCache) MigrationReport {
	report := MigrationReport{
		TotalSecrets:      len(migrations),
		CredentialUpdates: len(credUpdates),
	}

	migratedPaths := make(map[string]bool)
	for _, migration := range migrations {
		migratedPaths[migration.VaultPath] = true
		if migration.Error != nil {
			report.FailedSecrets++
			report.Errors = append(report.Errors, migration.Error)
		} else {
			report.SuccessfulSecrets++
			report.TotalFields += len(migration.CreatedFields)
		}
	}

	if cache != nil {
		for path, cached := range cache.Secrets {
			if migratedPaths[path] || cached.IsEmpty || cached.IsPlaceholder {
				continue
			}
			report.NotMigratedSecrets = append(report.NotMigratedSecrets, path)
		}
		sort.Strings(report.NotMigratedSecrets)
	}

	return report
}

// PrintReport outputs a human-readable summary of the migration
func PrintReport(report MigrationReport) {
	logrus.Info("================================================================================")
	logrus.Info("                         MIGRATION SUMMARY                                      ")
	logrus.Info("================================================================================")
	logrus.Infof("Vault secrets migrated to GSM:              %d", report.TotalSecrets)
	logrus.Infof("  Successful:                               %d", report.SuccessfulSecrets)
	logrus.Infof("  Failed:                                   %d", report.FailedSecrets)
	logrus.Infof("GSM fields created:                         %d", report.TotalFields)
	logrus.Infof("Bundles added to gsm-config.yaml:          %d", report.BundlesAddedToGSMConfig)
	logrus.Infof("Config entries removed from _config.yaml:  %d", report.ConfigEntriesRemovedFromConfig)
	logrus.Infof("Credential stanzas updated: %d", report.CredentialUpdates)

	if report.FailedSecrets > 0 {
		logrus.Info("================================================================================")
		logrus.Error("ERRORS:")
		for i, err := range report.Errors {
			logrus.Errorf("  [%d] %v", i+1, err)
		}
	}

	if len(report.NotMigratedSecrets) > 0 {
		logrus.Debug("================================================================================")
		logrus.Debugf("Vault secrets NOT migrated (%d):", len(report.NotMigratedSecrets))
		for _, path := range report.NotMigratedSecrets {
			logrus.Debugf("  %s", path)
		}
	}

	if len(report.IndexUpdateFailures) > 0 {
		logrus.Info("================================================================================")
		logrus.Errorf("INDEX UPDATE FAILURES (%d) -- secrets were still created in GSM, but the collection index could not be safely read/written, so it was left untouched. Re-run the migration once the underlying issue is resolved; it is safe to re-run in full", len(report.IndexUpdateFailures))
		for _, collection := range report.IndexUpdateFailures {
			logrus.Errorf("  %s", collection)
		}
	}

	logrus.Info("================================================================================")

	if report.FailedSecrets == 0 && len(report.IndexUpdateFailures) == 0 {
		logrus.Info("Migration completed successfully!")
	} else {
		logrus.Warnf("Migration completed with %d secret failures and %d collections needing an index re-run", report.FailedSecrets, len(report.IndexUpdateFailures))
	}
}

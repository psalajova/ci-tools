package vaultgsmmigration

import (
	"testing"
)

func TestExtractGroupFromVaultPath(t *testing.T) {
	tests := []struct {
		name        string
		vaultPath   string
		expected    string
		expectError bool
	}{
		{
			name:      "simple group",
			vaultPath: "kv/dptp/cloud.openshift.com-pull-secret",
			expected:  "cloud.openshift.com-pull-secret",
		},
		{
			name:      "nested group with one level",
			vaultPath: "kv/dptp/project/secret",
			expected:  "project/secret",
		},
		{
			name:      "nested group with multiple levels",
			vaultPath: "kv/team/org/project/secret",
			expected:  "org/project/secret",
		},
		{
			name:      "group with dots",
			vaultPath: "kv/dptp/service.example.com",
			expected:  "service.example.com",
		},
		{
			name:        "invalid path - too short",
			vaultPath:   "kv/dptp",
			expectError: true,
		},
		{
			name:        "invalid path - only mount",
			vaultPath:   "kv",
			expectError: true,
		},
		{
			name:      "simple secret name",
			vaultPath: "kv/dptp/simple",
			expected:  "simple",
		},
		{
			name:      "build_farm",
			vaultPath: "kv/dptp/build_farm",
			expected:  "build_farm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ExtractGroupFromVaultPath(tt.vaultPath)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if result != tt.expected {
				t.Errorf("expected %q but got %q", tt.expected, result)
			}
		})
	}
}

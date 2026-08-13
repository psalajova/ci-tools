package main

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/openshift/ci-tools/pkg/api"
)

func TestCollectSecretsFromTest(t *testing.T) {
	testCases := []struct {
		name     string
		test     api.TestStepConfiguration
		expected []credentialRef
	}{
		{
			name: "single secret field",
			test: api.TestStepConfiguration{
				Secret: &api.Secret{Name: "ossm-github-simple-job", MountPath: "/creds-github"},
			},
			expected: []credentialRef{{name: "ossm-github-simple-job"}},
		},
		{
			name: "multiple secrets in secrets list",
			test: api.TestStepConfiguration{
				Secrets: []*api.Secret{
					{Name: "ossm-github"},
					{Name: "acm-sonarcloud-token"},
				},
			},
			expected: []credentialRef{
				{name: "ossm-github"},
				{name: "acm-sonarcloud-token"},
			},
		},
		{
			name: "secret and secrets combined",
			test: api.TestStepConfiguration{
				Secret:  &api.Secret{Name: "a"},
				Secrets: []*api.Secret{{Name: "b"}},
			},
			expected: []credentialRef{
				{name: "a"},
				{name: "b"},
			},
		},
		{
			name: "already migrated secrets are skipped",
			test: api.TestStepConfiguration{
				Secrets: []*api.Secret{
					{Collection: "test-platform-infra", Group: "ossm-github"},
					{Bundle: "some-bundle"},
					{Name: "not-migrated"},
				},
			},
			expected: []credentialRef{{name: "not-migrated"}},
		},
		{
			name: "nil and empty-name secrets are skipped",
			test: api.TestStepConfiguration{
				Secrets: []*api.Secret{nil, {Name: ""}, {Name: "valid"}},
			},
			expected: []credentialRef{{name: "valid"}},
		},
		{
			name:     "no secrets",
			test:     api.TestStepConfiguration{},
			expected: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var credentials []credentialRef
			seen := make(map[string]bool)
			collectSecretsFromTest(&tc.test, &credentials, seen)

			if diff := cmp.Diff(tc.expected, credentials, cmp.AllowUnexported(credentialRef{})); diff != "" {
				t.Errorf("collected credentials differ from expected (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCollectSecretsFromTestDeduplicates(t *testing.T) {
	var credentials []credentialRef
	seen := make(map[string]bool)

	collectSecretsFromTest(&api.TestStepConfiguration{
		Secret:  &api.Secret{Name: "dup"},
		Secrets: []*api.Secret{{Name: "dup"}, {Name: "unique"}},
	}, &credentials, seen)

	expected := []credentialRef{
		{name: "dup"},
		{name: "unique"},
	}
	if diff := cmp.Diff(expected, credentials, cmp.AllowUnexported(credentialRef{}), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("expected dedup (-want +got):\n%s", diff)
	}
}

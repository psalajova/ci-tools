package gsmassmigration

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/openshift/ci-tools/pkg/api"
)

func TestExtractSecretRefs(t *testing.T) {
	bundleNames := sets.New("known-bundle")
	source := "test-file.yaml"

	tests := []struct {
		name     string
		secrets  []*api.Secret
		expected []gsmReference
	}{
		{
			name:    "nil secret is skipped",
			secrets: []*api.Secret{nil},
		},
		{
			name:    "known bundle is skipped",
			secrets: []*api.Secret{{Bundle: "known-bundle"}},
		},
		{
			name:    "unknown bundle is skipped with warning",
			secrets: []*api.Secret{{Bundle: "mystery"}},
		},
		{
			name:    "name-only secret is skipped",
			secrets: []*api.Secret{{Name: "old-style", MountPath: "/x"}},
		},
		{
			name:    "collection+group emits ref",
			secrets: []*api.Secret{{Collection: "my-coll", Group: "my-grp", MountPath: "/x"}},
			expected: []gsmReference{
				{Collection: "my-coll", Group: "my-grp", Source: source},
			},
		},
		{
			name:    "collection+group+field emits ref",
			secrets: []*api.Secret{{Collection: "c", Group: "g", Field: "f"}},
			expected: []gsmReference{
				{Collection: "c", Group: "g", Field: "f", Source: source},
			},
		},
		{
			name: "no varargs returns nil",
		},
		{
			name: "mixed inputs: only collection/group entries produce refs",
			secrets: []*api.Secret{
				nil,
				{Bundle: "known-bundle"},
				{Name: "old"},
				{Collection: "c", Group: "g"},
			},
			expected: []gsmReference{
				{Collection: "c", Group: "g", Source: source},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractSecretRefs(bundleNames, source, tc.secrets...)
			if diff := cmp.Diff(tc.expected, got); diff != "" {
				t.Errorf("extractSecretRefs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestExtractCredentialRefs(t *testing.T) {
	bundleNames := sets.New("known-bundle")
	source := "test-file.yaml"

	tests := []struct {
		name        string
		credentials []api.CredentialReference
		expected    []gsmReference
	}{
		{
			name:        "empty slice",
			credentials: []api.CredentialReference{},
		},
		{
			name:        "known bundle is skipped",
			credentials: []api.CredentialReference{{Bundle: "known-bundle"}},
		},
		{
			name:        "unknown bundle is skipped with warning",
			credentials: []api.CredentialReference{{Bundle: "unknown"}},
		},
		{
			name:        "namespace+name only is skipped",
			credentials: []api.CredentialReference{{Namespace: "ns", Name: "n"}},
		},
		{
			name:        "collection+group emits ref",
			credentials: []api.CredentialReference{{Collection: "c", Group: "g"}},
			expected: []gsmReference{
				{Collection: "c", Group: "g", Source: source},
			},
		},
		{
			name:        "collection+group+field emits ref",
			credentials: []api.CredentialReference{{Collection: "c", Group: "g", Field: "f"}},
			expected: []gsmReference{
				{Collection: "c", Group: "g", Field: "f", Source: source},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractCredentialRefs(tc.credentials, bundleNames, source)
			if diff := cmp.Diff(tc.expected, got); diff != "" {
				t.Errorf("extractCredentialRefs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCollectAllReferences(t *testing.T) {
	refs, bundleNames, err := collectAllReferences("testdata/credentials-migration/expected")
	if err != nil {
		t.Fatalf("collectAllReferences: %v", err)
	}

	expectedBundles := sets.New("hive-hive-credentials", "hypershift-agent-ibmz-credentials")
	if diff := cmp.Diff(expectedBundles, bundleNames); diff != "" {
		t.Errorf("bundleNames mismatch (-want +got):\n%s", diff)
	}

	type refKey struct{ collection, group, field string }
	seen := sets.New[refKey]()
	for _, ref := range refs {
		seen.Insert(refKey{ref.Collection, ref.Group, ref.Field})
	}

	// Bundle definitions from gsm-config.yaml
	gsmConfigRefs := []refKey{
		{"test-platform-infra", "build_farm", "sa--dot--hive--dot--hosted-mgmt--dot--config"},
		{"test-platform-infra", "build_farm", "sa--dot--hive--dot--hosted-mgmt--dot--token--dot--txt"},
		{"hypershift-agent-ibmz-credentials", "abi-pull-secret", ""},
		{"hypershift-agent-ibmz-credentials", "brew-token", ""},
		{"hypershift-agent-ibmz-credentials", "httpd-vsi-ip", ""},
		{"hypershift-agent-ibmz-credentials", "httpd-vsi-key", ""},
		{"hypershift-agent-ibmz-credentials", "httpd-vsi-pub-key", ""},
		{"hypershift-agent-ibmz-credentials", "ibmcloud-apikey", ""},
		{"hypershift-agent-ibmz-credentials", "ibmcloud-zcomm-apikey", ""},
		{"hypershift-agent-ibmz-credentials", "quay-token", ""},
	}

	// Secrets stanzas from ci-operator configs (test-secrets-test-repo-master.yaml)
	secretsStanzaRefs := []refKey{
		{"my-test-collection", "my-test-secret", ""},
		{"another-collection", "another-test-secret", ""},
		{"some-collection", "some-group", ""},
	}

	// Credentials stanzas from step-registry refs
	stepRegistryRefs := []refKey{
		{"loki-ci", "loki-prod-collector-test-secret", ""},
		{"vsphere-vmc", "ibmcloud8", ""},
		{"vsphere", "ibmcloud/ci", ""},
		{"openshift-qe", "vsphere-devqe", ""},
		{"telcov10n-ci", "ansible/ansible_group_all", ""},
		{"telcov10n-ci", "teams/network/clusters/kni-qe-92/ansible_group_masters", ""},
		{"telcov10n", "kni-qe-92/bastion--dot--kni-qe-92--dot--telcoqe--dot--eng--dot--rdu2--dot--dc--dot--redhat--dot--com", ""},
	}

	for _, group := range []struct {
		label string
		keys  []refKey
	}{
		{"gsm-config.yaml bundle definitions", gsmConfigRefs},
		{"ci-operator config secrets stanzas", secretsStanzaRefs},
		{"step-registry credentials stanzas", stepRegistryRefs},
	} {
		for _, key := range group.keys {
			if !seen.Has(key) {
				t.Errorf("%s: expected ref {collection=%q, group=%q, field=%q} not found",
					group.label, key.collection, key.group, key.field)
			}
		}
	}
}

package privateorg

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
)

func TestMirroredRepoName(t *testing.T) {
	flattenedOrgs := sets.New[string](DefaultFlattenOrgs...)

	testCases := []struct {
		name     string
		org      string
		repo     string
		expected string
	}{
		{
			name:     "default flattened org keeps original repo name",
			org:      "openshift",
			repo:     "installer",
			expected: "installer",
		},
		{
			name:     "another default flattened org keeps original repo name",
			org:      "openshift-eng",
			repo:     "ci-test-mapping",
			expected: "ci-test-mapping",
		},
		{
			name:     "non-default org uses prefixed repo name",
			org:      "migtools",
			repo:     "filebrowser",
			expected: "migtools-filebrowser",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := MirroredRepoName(tc.org, tc.repo, flattenedOrgs)
			if got != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, got)
			}
		})
	}
}

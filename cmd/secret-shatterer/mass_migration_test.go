package main

import "testing"

func TestNamespaceMatches(t *testing.T) {
	testCases := []struct {
		name             string
		ns               string
		targetNamespaces string
		expected         bool
	}{
		{name: "single match", ns: "ci", targetNamespaces: "ci", expected: true},
		{name: "single no match", ns: "ci", targetNamespaces: "test-credentials", expected: false},
		{name: "comma-separated match", ns: "test-credentials", targetNamespaces: "ci,test-credentials", expected: true},
		{name: "comma-separated with spaces match", ns: "test-credentials", targetNamespaces: "ci, test-credentials", expected: true},
		{name: "comma-separated no match", ns: "kube-system", targetNamespaces: "ci,test-credentials", expected: false},
		{name: "empty namespace no match", ns: "", targetNamespaces: "ci", expected: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := namespaceMatches(tc.ns, tc.targetNamespaces); got != tc.expected {
				t.Errorf("namespaceMatches(%q, %q) = %v, want %v", tc.ns, tc.targetNamespaces, got, tc.expected)
			}
		})
	}
}

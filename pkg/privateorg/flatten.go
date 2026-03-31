package privateorg

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/sets"
)

// DefaultFlattenOrgs contains organizations whose repos should not have org
// prefix by default for backwards compatibility.
var DefaultFlattenOrgs = []string{
	"openshift",
	"openshift-eng",
	"operator-framework",
	"redhat-cne",
	"openshift-assisted",
	"ViaQ",
}

// MirroredRepoName returns the repo name as it appears in the private org.
// For flattened orgs, the repo name is unchanged; otherwise it is prefixed
// with the source org (e.g. "migtools-filebrowser").
func MirroredRepoName(org, repo string, flattenedOrgs sets.Set[string]) string {
	if flattenedOrgs.Has(org) {
		return repo
	}
	return fmt.Sprintf("%s-%s", org, repo)
}

// ArrayFlags implements flag.Value for repeated string flags.
type ArrayFlags []string

func (i *ArrayFlags) String() string {
	return fmt.Sprintf("%v", *i)
}

func (i *ArrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

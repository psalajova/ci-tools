package gsmproxy

import (
	"errors"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"

	"github.com/openshift/ci-tools/pkg/group"
)

// ErrForbidden is returned whenever a caller may not see a collection, whatever the reason. The
// reason stays in the log so the response cannot be used to probe the configuration.
var ErrForbidden = errors.New("forbidden")

// Authorizer answers whether a kerberos id may read the secrets of a secret collection.
type Authorizer struct {
	// collectionOwners maps a secret collection to the rover groups that own it.
	collectionOwners map[string]sets.Set[string]
	// roverGroupMembers maps a rover group to the users it contains (their kerberos IDs).
	roverGroupMembers map[string]sets.Set[string]
}

// AuthorizeCollectionAccess checks if a kerberosID belongs to any rover group owning the secret collection.
func (a *Authorizer) AuthorizeCollectionAccess(kerberosID, collection string) error {
	collectionOwners, claimed := a.collectionOwners[collection]
	if !claimed {
		return fmt.Errorf("%w: collection %q is not owned by any rover group", ErrForbidden, collection)
	}
	for _, rg := range sets.List(collectionOwners) {
		if a.roverGroupMembers[rg].Has(kerberosID) {
			return nil
		}
	}
	return fmt.Errorf("%w: %q is not a member of any rover group owning collection %q", ErrForbidden, kerberosID, collection)
}

// NewAuthorizer indexes the rover group config and the resolved group memberships.
func NewAuthorizer(config *group.Config, members map[string][]string) *Authorizer {
	a := &Authorizer{
		collectionOwners:  map[string]sets.Set[string]{},
		roverGroupMembers: map[string]sets.Set[string]{},
	}
	for name, target := range config.Groups {
		// Unclaimed groups are holding areas for collections no team owns.
		if target.Unclaimed {
			continue
		}
		for _, collection := range target.SecretCollections {
			if a.collectionOwners[collection] == nil {
				a.collectionOwners[collection] = sets.New[string]()
			}
			a.collectionOwners[collection].Insert(name)
		}
	}
	for name, ids := range members {
		a.roverGroupMembers[name] = sets.New[string](ids...)
	}
	return a
}

// LoadAuthorizer reads _config.yaml and groups.yaml from disk and indexes them.
//
// This is called once per request rather than cached, because both files arrive as mounted
// ConfigMaps that change underneath the process: groups.yaml from a daily job, _config.yaml from
// config_updater. Reading them per request costs ~5.6ms for the 18KB and 59KB files in production
// and means a newly added group member is authorized as soon as the kubelet has rewritten the
// mount, with no reload machinery and no window where the process is serving stale membership.
func LoadAuthorizer(configPath, groupsPath string) (*Authorizer, error) {
	config, err := group.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load rover groups config: %w", err)
	}
	if len(config.Groups) == 0 {
		return nil, errors.New("rover groups config declares no groups")
	}
	groupsData, err := os.ReadFile(groupsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read groups file: %w", err)
	}
	var members map[string][]string
	if err := yaml.Unmarshal(groupsData, &members); err != nil {
		return nil, fmt.Errorf("failed to parse groups file: %w", err)
	}
	// An empty groups file would authorize nobody rather than everybody, but it almost certainly
	// means a failed LDAP sync, and silently denying every request is worse than refusing to start.
	if len(members) == 0 {
		return nil, errors.New("groups file declares no groups")
	}
	return NewAuthorizer(config, members), nil
}

package main

import "testing"

// gcloudClientID is the "aud" claim on an identity token printed by a human who ran
// "gcloud auth login". It is gcloud's own public OAuth client ID, shared by every installation,
// and a user account cannot substitute anything else for it.
const gcloudClientID = "32555940559.apps.googleusercontent.com"

func TestOptionsValidate(t *testing.T) {
	valid := func() *options {
		return &options{
			audience:                  gcloudClientID,
			project:                   "openshift-ci-secrets",
			syncRoverGroupsConfigPath: "/etc/sync-rover-groups-config/_config.yaml",
			groupsKerberosIdsPath:     "/etc/sync-rover-groups/groups.yaml",
			credentialsFile:           "/etc/gcp/credentials.json",
		}
	}

	for _, tc := range []struct {
		name        string
		mutate      func(*options)
		expectError bool
	}{
		{name: "a fully specified set of options is valid"},
		{name: "the audience is required", mutate: func(o *options) { o.audience = "" }, expectError: true},
		{name: "the config file is required", mutate: func(o *options) { o.syncRoverGroupsConfigPath = "" }, expectError: true},
		{name: "the groups file is required", mutate: func(o *options) { o.groupsKerberosIdsPath = "" }, expectError: true},
		{name: "the credentials file is required", mutate: func(o *options) { o.credentialsFile = "" }, expectError: true},
		{name: "the project is required", mutate: func(o *options) { o.project = "" }, expectError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := valid()
			if tc.mutate != nil {
				tc.mutate(o)
			}
			err := o.validate()
			if tc.expectError && err == nil {
				t.Error("expected an error")
			}
			if !tc.expectError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

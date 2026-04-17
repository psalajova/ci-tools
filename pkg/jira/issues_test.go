package jira

import (
	"errors"
	"testing"

	"github.com/andygrunwald/go-jira"
	"github.com/google/go-cmp/cmp"
	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
	"github.com/trivago/tgo/tcontainer"
)

type userResponse struct {
	user *slack.User
	err  error
}

type fakeSlackClient struct {
	userBehavior  map[string]userResponse
	unwantedUsers []string
}

func (f *fakeSlackClient) GetUserInfo(user string) (*slack.User, error) {
	response, registered := f.userBehavior[user]
	if !registered {
		f.unwantedUsers = append(f.unwantedUsers, user)
		return nil, errors.New("no such user behavior in fake")
	}
	delete(f.userBehavior, user)
	return response.user, response.err
}

func (f *fakeSlackClient) Validate(t *testing.T) {
	for user := range f.userBehavior {
		t.Errorf("fake info getter did not get user request: %v", user)
	}
	for _, user := range f.unwantedUsers {
		t.Errorf("fake info getter got unwanted user request: %v", user)
	}
}

func TestRequesterSuffix(t *testing.T) {
	var testCases = []struct {
		name           string
		filer          filer
		reporter       string
		expectedSuffix string
	}{
		{
			name: "gets Slack user for suffix",
			filer: filer{
				slackClient: &fakeSlackClient{
					userBehavior: map[string]userResponse{
						"skuznets": {user: &slack.User{RealName: "Steve Kuznetsov", ID: "slackIdentifier"}},
					},
					unwantedUsers: []string{},
				},
			},
			reporter:       "skuznets",
			expectedSuffix: "Slack user [Steve Kuznetsov|https://redhat-internal.slack.com/team/slackIdentifier]",
		},
		{
			name: "falls back when Slack lookup fails",
			filer: filer{
				slackClient: &fakeSlackClient{
					userBehavior: map[string]userResponse{
						"skuznets": {err: errors.New("oops")},
					},
					unwantedUsers: []string{},
				},
			},
			reporter:       "skuznets",
			expectedSuffix: "[a Slack user|https://redhat-internal.slack.com/team/skuznets]",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			suffix := testCase.filer.requesterSuffix(testCase.reporter, logrus.WithField("test", testCase.name))
			if diff := cmp.Diff(testCase.expectedSuffix, suffix); diff != "" {
				t.Errorf("%s: did not get correct suffix: %v", testCase.name, diff)
			}
		})
	}
}

func TestFindClosingTransition(t *testing.T) {
	testCases := []struct {
		name        string
		resolution  string
		transitions []jira.Transition
		expectedID  string
	}{
		{
			name:       "prefer exact transition name match on resolution",
			resolution: "Done",
			transitions: []jira.Transition{
				{ID: "1", Name: "Close Issue", To: jira.Status{Name: "Closed"}},
				{ID: "2", Name: "Done", To: jira.Status{Name: "Done"}},
			},
			expectedID: "2",
		},
		{
			name:       "fallback to exact destination status on resolution",
			resolution: "Done",
			transitions: []jira.Transition{
				{ID: "1", Name: "Close Issue", To: jira.Status{Name: "Done"}},
			},
			expectedID: "1",
		},
		{
			name:       "fallback to known closing transition names",
			resolution: "Done",
			transitions: []jira.Transition{
				{ID: "1", Name: "Close Issue", To: jira.Status{Name: "In Progress"}},
			},
			expectedID: "1",
		},
		{
			name:       "fallback to known closing statuses",
			resolution: "Done",
			transitions: []jira.Transition{
				{ID: "1", Name: "Random Transition", To: jira.Status{Name: "Resolved"}},
			},
			expectedID: "1",
		},
		{
			name:       "no matching close transition",
			resolution: "Done",
			transitions: []jira.Transition{
				{ID: "1", Name: "Move", To: jira.Status{Name: "In Progress"}},
			},
			expectedID: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findClosingTransition(tc.transitions, tc.resolution); got != tc.expectedID {
				t.Fatalf("expected transition %q, got %q", tc.expectedID, got)
			}
		})
	}
}

func TestSetActivityTypeField(t *testing.T) {
	file := filer{activityTypeFieldKey: "customfield_12345", activityTypeValueID: "20001"}
	fields := &jira.IssueFields{}

	file.setActivityTypeField(fields)

	if fields.Unknowns == nil {
		t.Fatalf("expected unknowns to be initialized")
	}
	got, found := fields.Unknowns["customfield_12345"]
	if !found {
		t.Fatalf("expected activity type custom field to be set")
	}
	if diff := cmp.Diff(tcontainer.MarshalMap{"id": "20001"}, got); diff != "" {
		t.Fatalf("unexpected activity type field value: %s", diff)
	}
}

func TestSetActivityTypeFieldNoKey(t *testing.T) {
	file := filer{activityTypeFieldKey: "", activityTypeValueID: "20001"}
	fields := &jira.IssueFields{Unknowns: tcontainer.NewMarshalMap()}

	file.setActivityTypeField(fields)

	if len(fields.Unknowns) != 0 {
		t.Fatalf("expected unknowns to stay unchanged when field key is empty")
	}
}

func TestActivityTypeOptionID(t *testing.T) {
	fields := tcontainer.NewMarshalMap()
	fields["customfield_12345"] = tcontainer.NewMarshalMap()
	fields["customfield_12345"].(tcontainer.MarshalMap)["allowedValues"] = []interface{}{
		tcontainer.MarshalMap{"id": "20000", "value": "Other"},
		tcontainer.MarshalMap{"id": "20001", "value": activityTypeValue},
	}

	got, err := activityTypeOptionID(fields, "customfield_12345", activityTypeValue)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "20001" {
		t.Fatalf("expected option ID 20001, got %s", got)
	}
}

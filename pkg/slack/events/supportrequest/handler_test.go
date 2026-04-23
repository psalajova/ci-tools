package supportrequest

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	ajira "github.com/andygrunwald/go-jira"
	"github.com/google/go-cmp/cmp"
	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	ciJira "github.com/openshift/ci-tools/pkg/jira"
)

type fakeClient struct {
	repliesByTS      map[string][]slack.Message
	pagedRepliesByTS map[string][][]slack.Message
	historyByTS      map[string]*slack.GetConversationHistoryResponse
	permalink        string
	postCount        int
	repliesCalls     int
}

func (f *fakeClient) PostMessage(channelID string, options ...slack.MsgOption) (string, string, error) {
	f.postCount++
	return channelID, "123.456", nil
}

func (f *fakeClient) GetConversationReplies(params *slack.GetConversationRepliesParameters) (msgs []slack.Message, hasMore bool, nextCursor string, err error) {
	f.repliesCalls++
	if pages, ok := f.pagedRepliesByTS[params.Timestamp]; ok {
		pageIdx := 0
		if params.Cursor != "" {
			if parsed, err := strconv.Atoi(params.Cursor); err == nil {
				pageIdx = parsed
			}
		}
		if pageIdx >= len(pages) {
			return nil, false, "", nil
		}
		hasMore = pageIdx+1 < len(pages)
		if hasMore {
			nextCursor = strconv.Itoa(pageIdx + 1)
		}
		return pages[pageIdx], hasMore, nextCursor, nil
	}
	return f.repliesByTS[params.Timestamp], false, "", nil
}

func (f *fakeClient) GetConversationHistory(params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	if f.historyByTS == nil {
		return &slack.GetConversationHistoryResponse{}, nil
	}
	if history, ok := f.historyByTS[params.Latest]; ok {
		return history, nil
	}
	return &slack.GetConversationHistoryResponse{}, nil
}

func (f *fakeClient) GetPermalink(params *slack.PermalinkParameters) (string, error) {
	return f.permalink, nil
}

type fakeFiler struct {
	fileCalls      []ciJira.IssueRequest
	closeCalls     []ciJira.CloseIssueRequest
	setStatusCalls []ciJira.SetIssueStatusRequest
	issue          *ajira.Issue
	closeResult    bool
}

func (f *fakeFiler) FileIssue(issueType, title, description, reporter, activityType string, logger *logrus.Entry) (*ajira.Issue, error) {
	f.fileCalls = append(f.fileCalls, ciJira.IssueRequest{
		IssueType:    issueType,
		Title:        title,
		Description:  description,
		Reporter:     reporter,
		ActivityType: activityType,
	})
	return f.issue, nil
}

func (f *fakeFiler) CloseIssue(issueKey, resolution string, logger *logrus.Entry) (bool, error) {
	f.closeCalls = append(f.closeCalls, ciJira.CloseIssueRequest{
		IssueKey:   issueKey,
		Resolution: resolution,
	})
	if !f.closeResult {
		return false, nil
	}
	return true, nil
}

func (f *fakeFiler) SetIssueStatus(issueKey, status string, logger *logrus.Entry) error {
	f.setStatusCalls = append(f.setStatusCalls, ciJira.SetIssueStatusRequest{
		IssueKey: issueKey,
		Status:   status,
	})
	return nil
}

func TestCreateSupportRequest(t *testing.T) {
	channelID := "C123"
	threadTS := "100.100"
	client := &fakeClient{
		repliesByTS: map[string][]slack.Message{
			threadTS: {
				{Msg: slack.Msg{Timestamp: threadTS}},
				{Msg: slack.Msg{Timestamp: "100.101"}},
				{Msg: slack.Msg{Timestamp: "100.102"}},
				{Msg: slack.Msg{Timestamp: "100.103"}},
				{Msg: slack.Msg{Timestamp: "100.104"}},
				{Msg: slack.Msg{Timestamp: "100.105"}},
			},
		},
		permalink: "https://example.slack.com/archives/C123/p100100",
	}
	filer := &fakeFiler{issue: &ajira.Issue{Key: "DPTP-123"}}

	handler := Handler(client, filer, channelID, 5)
	handled, err := handler.Handle(&slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				Channel:         channelID,
				ChannelType:     "channel",
				ThreadTimeStamp: threadTS,
				User:            "U123",
			},
		},
	}, logrus.NewEntry(logrus.StandardLogger()))
	if err != nil {
		t.Fatalf("did not expect error: %v", err)
	}
	if !handled {
		t.Fatalf("expected message to be handled")
	}
	if len(filer.fileCalls) != 1 {
		t.Fatalf("expected exactly one jira file call, got %d", len(filer.fileCalls))
	}
	if got := filer.fileCalls[0].IssueType; got != ciJira.IssueTypeTask {
		t.Fatalf("expected issue type %s, got %s", ciJira.IssueTypeTask, got)
	}
	if diff := cmp.Diff([]ciJira.SetIssueStatusRequest{{IssueKey: "DPTP-123", Status: ciJira.StatusInProgress}}, filer.setStatusCalls); diff != "" {
		t.Fatalf("unexpected set-status calls: %s", diff)
	}
	if !strings.Contains(filer.fileCalls[0].Description, client.permalink) {
		t.Fatalf("expected description to include permalink, got: %s", filer.fileCalls[0].Description)
	}
	if client.postCount != 1 {
		t.Fatalf("expected one posted message, got %d", client.postCount)
	}
}

func TestCreateSupportRequestWithCanonicalRootTimestamp(t *testing.T) {
	channelID := "C123"
	threadTS := "100.100"
	client := &fakeClient{
		repliesByTS: map[string][]slack.Message{
			threadTS: {
				{Msg: slack.Msg{Timestamp: "100.100000", ThreadTimestamp: threadTS}},
				{Msg: slack.Msg{Timestamp: "100.101", User: "U1"}},
				{Msg: slack.Msg{Timestamp: "100.102", User: "U2"}},
				{Msg: slack.Msg{Timestamp: "100.103", User: "U3"}},
				{Msg: slack.Msg{Timestamp: "100.104", User: "U4"}},
				{Msg: slack.Msg{Timestamp: "100.105", User: "U5"}},
			},
		},
		permalink: "https://example.slack.com/archives/C123/p100100",
	}
	filer := &fakeFiler{issue: &ajira.Issue{Key: "DPTP-123"}}

	handler := Handler(client, filer, channelID, 5)
	handled, err := handler.Handle(&slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				Channel:         channelID,
				ChannelType:     "channel",
				ThreadTimeStamp: threadTS,
				User:            "U123",
			},
		},
	}, logrus.NewEntry(logrus.StandardLogger()))
	if err != nil {
		t.Fatalf("did not expect error: %v", err)
	}
	if !handled {
		t.Fatalf("expected message to be handled")
	}
	if len(filer.fileCalls) != 1 {
		t.Fatalf("expected exactly one jira file call, got %d", len(filer.fileCalls))
	}
}

func TestNoDuplicateSupportRequest(t *testing.T) {
	channelID := "C123"
	threadTS := "100.100"
	client := &fakeClient{
		repliesByTS: map[string][]slack.Message{
			threadTS: {
				{Msg: slack.Msg{Timestamp: threadTS}},
				{Msg: slack.Msg{Text: fmt.Sprintf("%s <https://issues.redhat.com/browse/DPTP-42|DPTP-42>", supportRequestPrefix)}},
				{Msg: slack.Msg{Timestamp: "100.102"}},
				{Msg: slack.Msg{Timestamp: "100.103"}},
				{Msg: slack.Msg{Timestamp: "100.104"}},
				{Msg: slack.Msg{Timestamp: "100.105"}},
			},
		},
	}
	filer := &fakeFiler{issue: &ajira.Issue{Key: "DPTP-123"}}

	handler := Handler(client, filer, channelID, 5)
	handled, err := handler.Handle(&slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				Channel:         channelID,
				ChannelType:     "channel",
				ThreadTimeStamp: threadTS,
				User:            "U123",
			},
		},
	}, logrus.NewEntry(logrus.StandardLogger()))
	if err != nil {
		t.Fatalf("did not expect error: %v", err)
	}
	if handled {
		t.Fatalf("expected handler not to consume duplicate support request")
	}
	if len(filer.fileCalls) != 0 {
		t.Fatalf("expected zero jira file calls, got %d", len(filer.fileCalls))
	}
}

func TestProcessedThreadCacheSkipsRepeatedEligibilityScans(t *testing.T) {
	channelID := "C123"
	threadTS := "100.100"
	client := &fakeClient{
		repliesByTS: map[string][]slack.Message{
			threadTS: {
				{Msg: slack.Msg{Timestamp: threadTS}},
				{Msg: slack.Msg{Text: fmt.Sprintf("%s <https://issues.redhat.com/browse/DPTP-42|DPTP-42>", supportRequestPrefix)}},
				{Msg: slack.Msg{Timestamp: "100.102"}},
				{Msg: slack.Msg{Timestamp: "100.103"}},
				{Msg: slack.Msg{Timestamp: "100.104"}},
				{Msg: slack.Msg{Timestamp: "100.105"}},
			},
		},
	}
	filer := &fakeFiler{issue: &ajira.Issue{Key: "DPTP-123"}}
	handler := Handler(client, filer, channelID, 5)

	event := &slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				Channel:         channelID,
				ChannelType:     "channel",
				ThreadTimeStamp: threadTS,
				User:            "U123",
			},
		},
	}
	logger := logrus.NewEntry(logrus.StandardLogger())
	handled, err := handler.Handle(event, logger)
	if err != nil {
		t.Fatalf("did not expect error on first event: %v", err)
	}
	if handled {
		t.Fatalf("expected first event to be skipped due to existing jira marker")
	}
	handled, err = handler.Handle(event, logger)
	if err != nil {
		t.Fatalf("did not expect error on second event: %v", err)
	}
	if handled {
		t.Fatalf("expected second event to be skipped from cache")
	}
	if client.repliesCalls != 1 {
		t.Fatalf("expected one replies fetch across both events, got %d", client.repliesCalls)
	}
}

func TestBotMessageEventIgnoredWithoutThreadFetch(t *testing.T) {
	channelID := "C123"
	threadTS := "100.100"
	client := &fakeClient{
		repliesByTS: map[string][]slack.Message{
			threadTS: {
				{Msg: slack.Msg{Timestamp: threadTS}},
			},
		},
	}
	filer := &fakeFiler{issue: &ajira.Issue{Key: "DPTP-123"}}
	handler := Handler(client, filer, channelID, 5)

	handled, err := handler.Handle(&slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				Channel:         channelID,
				ChannelType:     "channel",
				ThreadTimeStamp: threadTS,
				SubType:         "bot_message",
				BotID:           "B123",
			},
		},
	}, logrus.NewEntry(logrus.StandardLogger()))
	if err != nil {
		t.Fatalf("did not expect error: %v", err)
	}
	if handled {
		t.Fatalf("expected bot message event not to be handled")
	}
	if len(filer.fileCalls) != 0 {
		t.Fatalf("expected zero jira file calls, got %d", len(filer.fileCalls))
	}
	if client.repliesCalls != 0 {
		t.Fatalf("expected zero replies fetches for bot event, got %d", client.repliesCalls)
	}
}

func TestNoSupportRequestWhenRootIsClosed(t *testing.T) {
	channelID := "C123"
	threadTS := "100.100"
	client := &fakeClient{
		repliesByTS: map[string][]slack.Message{
			threadTS: {
				{
					Msg: slack.Msg{
						Timestamp: threadTS,
						Reactions: []slack.ItemReaction{{Name: closedReaction}},
					},
				},
				{Msg: slack.Msg{Timestamp: "100.101"}},
				{Msg: slack.Msg{Timestamp: "100.102"}},
				{Msg: slack.Msg{Timestamp: "100.103"}},
				{Msg: slack.Msg{Timestamp: "100.104"}},
				{Msg: slack.Msg{Timestamp: "100.105"}},
			},
		},
	}
	filer := &fakeFiler{issue: &ajira.Issue{Key: "DPTP-123"}}

	handler := Handler(client, filer, channelID, 5)
	handled, err := handler.Handle(&slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				Channel:         channelID,
				ChannelType:     "channel",
				ThreadTimeStamp: threadTS,
				User:            "U123",
			},
		},
	}, logrus.NewEntry(logrus.StandardLogger()))
	if err != nil {
		t.Fatalf("did not expect error: %v", err)
	}
	if handled {
		t.Fatalf("expected handler not to create support request for closed root thread")
	}
	if len(filer.fileCalls) != 0 {
		t.Fatalf("expected zero jira file calls, got %d", len(filer.fileCalls))
	}
}

func TestNoSupportRequestWhenRootIsNotApplicable(t *testing.T) {
	channelID := "C123"
	threadTS := "100.100"
	client := &fakeClient{
		repliesByTS: map[string][]slack.Message{
			threadTS: {
				{
					Msg: slack.Msg{
						Timestamp: threadTS,
						Reactions: []slack.ItemReaction{{Name: notApplicableReaction}},
					},
				},
				{Msg: slack.Msg{Timestamp: "100.101"}},
				{Msg: slack.Msg{Timestamp: "100.102"}},
				{Msg: slack.Msg{Timestamp: "100.103"}},
				{Msg: slack.Msg{Timestamp: "100.104"}},
				{Msg: slack.Msg{Timestamp: "100.105"}},
			},
		},
	}
	filer := &fakeFiler{issue: &ajira.Issue{Key: "DPTP-123"}}

	handler := Handler(client, filer, channelID, 5)
	handled, err := handler.Handle(&slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				Channel:         channelID,
				ChannelType:     "channel",
				ThreadTimeStamp: threadTS,
				User:            "U123",
			},
		},
	}, logrus.NewEntry(logrus.StandardLogger()))
	if err != nil {
		t.Fatalf("did not expect error: %v", err)
	}
	if handled {
		t.Fatalf("expected handler not to create support request for not-applicable root thread")
	}
	if len(filer.fileCalls) != 0 {
		t.Fatalf("expected zero jira file calls, got %d", len(filer.fileCalls))
	}
}

func TestNoSupportRequestWhenOnlyBotRepliesCrossThreshold(t *testing.T) {
	channelID := "C123"
	threadTS := "100.100"
	client := &fakeClient{
		repliesByTS: map[string][]slack.Message{
			threadTS: {
				{Msg: slack.Msg{Timestamp: threadTS, User: "U123"}},
				{Msg: slack.Msg{Timestamp: "100.101", User: "U124"}},
				{Msg: slack.Msg{Timestamp: "100.102", User: "U125"}},
				{Msg: slack.Msg{Timestamp: "100.103", BotID: "B123", SubType: "bot_message"}},
				{Msg: slack.Msg{Timestamp: "100.104", BotID: "B124", SubType: "bot_message"}},
				{Msg: slack.Msg{Timestamp: "100.105", BotID: "B125", SubType: "bot_message"}},
			},
		},
	}
	filer := &fakeFiler{issue: &ajira.Issue{Key: "DPTP-123"}}

	handler := Handler(client, filer, channelID, 5)
	handled, err := handler.Handle(&slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				Channel:         channelID,
				ChannelType:     "channel",
				ThreadTimeStamp: threadTS,
				User:            "U123",
			},
		},
	}, logrus.NewEntry(logrus.StandardLogger()))
	if err != nil {
		t.Fatalf("did not expect error: %v", err)
	}
	if handled {
		t.Fatalf("expected handler not to create support request when only bot messages cross threshold")
	}
	if len(filer.fileCalls) != 0 {
		t.Fatalf("expected zero jira file calls, got %d", len(filer.fileCalls))
	}
}

func TestNoSupportRequestWhenRootStartsWithShipHelp(t *testing.T) {
	channelID := "C123"
	threadTS := "100.100"
	client := &fakeClient{
		repliesByTS: map[string][]slack.Message{
			threadTS: {
				{Msg: slack.Msg{Timestamp: threadTS, Text: "@ship-help please assist"}},
				{Msg: slack.Msg{Timestamp: "100.101"}},
				{Msg: slack.Msg{Timestamp: "100.102"}},
				{Msg: slack.Msg{Timestamp: "100.103"}},
				{Msg: slack.Msg{Timestamp: "100.104"}},
				{Msg: slack.Msg{Timestamp: "100.105"}},
			},
		},
	}
	filer := &fakeFiler{issue: &ajira.Issue{Key: "DPTP-123"}}

	handler := Handler(client, filer, channelID, 5)
	handled, err := handler.Handle(&slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				Channel:         channelID,
				ChannelType:     "channel",
				ThreadTimeStamp: threadTS,
				User:            "U123",
			},
		},
	}, logrus.NewEntry(logrus.StandardLogger()))
	if err != nil {
		t.Fatalf("did not expect error: %v", err)
	}
	if handled {
		t.Fatalf("expected handler not to create support request for @ship-help root thread")
	}
	if len(filer.fileCalls) != 0 {
		t.Fatalf("expected zero jira file calls, got %d", len(filer.fileCalls))
	}
}

func TestCloseSupportRequest(t *testing.T) {
	channelID := "C123"
	threadTS := "100.100"
	client := &fakeClient{
		repliesByTS: map[string][]slack.Message{
			threadTS: {
				{Msg: slack.Msg{Timestamp: threadTS}},
				{Msg: slack.Msg{Text: fmt.Sprintf("%s <https://issues.redhat.com/browse/DPTP-42|DPTP-42>", supportRequestPrefix)}},
			},
		},
		historyByTS: map[string]*slack.GetConversationHistoryResponse{
			threadTS: {
				Messages: []slack.Message{
					{Msg: slack.Msg{Timestamp: threadTS}},
				},
			},
		},
	}
	filer := &fakeFiler{closeResult: true}
	handler := Handler(client, filer, channelID, 5)
	handled, err := handler.Handle(&slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.ReactionAddedEvent{
				Reaction: closedReaction,
				Item: slackevents.Item{
					Channel:   channelID,
					Timestamp: threadTS,
				},
			},
		},
	}, logrus.NewEntry(logrus.StandardLogger()))
	if err != nil {
		t.Fatalf("did not expect error: %v", err)
	}
	if !handled {
		t.Fatalf("expected reaction to be handled")
	}
	if len(filer.closeCalls) != 1 {
		t.Fatalf("expected exactly one jira close call, got %d", len(filer.closeCalls))
	}
	if diff := cmp.Diff(ciJira.CloseIssueRequest{IssueKey: "DPTP-42", Resolution: ciJira.ResolutionDone}, filer.closeCalls[0]); diff != "" {
		t.Fatalf("unexpected close issue call: %s", diff)
	}
}

func TestCloseSupportRequestWithCanonicalRootTimestamp(t *testing.T) {
	channelID := "C123"
	threadTS := "100.100"
	client := &fakeClient{
		repliesByTS: map[string][]slack.Message{
			threadTS: {
				{Msg: slack.Msg{Timestamp: "100.100000", ThreadTimestamp: threadTS}},
				{Msg: slack.Msg{Text: fmt.Sprintf("%s <https://issues.redhat.com/browse/DPTP-42|DPTP-42>", supportRequestPrefix)}},
			},
		},
		historyByTS: map[string]*slack.GetConversationHistoryResponse{
			threadTS: {
				Messages: []slack.Message{
					{Msg: slack.Msg{Timestamp: threadTS}},
				},
			},
		},
	}
	filer := &fakeFiler{closeResult: true}
	handler := Handler(client, filer, channelID, 5)
	handled, err := handler.Handle(&slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.ReactionAddedEvent{
				Reaction: closedReaction,
				Item: slackevents.Item{
					Channel:   channelID,
					Timestamp: threadTS,
				},
			},
		},
	}, logrus.NewEntry(logrus.StandardLogger()))
	if err != nil {
		t.Fatalf("did not expect error: %v", err)
	}
	if !handled {
		t.Fatalf("expected reaction to be handled")
	}
	if len(filer.closeCalls) != 1 {
		t.Fatalf("expected exactly one jira close call, got %d", len(filer.closeCalls))
	}
}

func TestCloseSupportRequestScansByPages(t *testing.T) {
	channelID := "C123"
	threadTS := "100.100"
	client := &fakeClient{
		pagedRepliesByTS: map[string][][]slack.Message{
			threadTS: {
				{
					{Msg: slack.Msg{Timestamp: threadTS}},
					{Msg: slack.Msg{Text: "noise-1"}},
				},
				{
					{Msg: slack.Msg{Text: "noise-2"}},
					{Msg: slack.Msg{Text: fmt.Sprintf("%s <https://issues.redhat.com/browse/DPTP-99|DPTP-99>", supportRequestPrefix)}},
				},
			},
		},
		historyByTS: map[string]*slack.GetConversationHistoryResponse{
			threadTS: {
				Messages: []slack.Message{
					{Msg: slack.Msg{Timestamp: threadTS}},
				},
			},
		},
	}
	filer := &fakeFiler{closeResult: true}
	handler := Handler(client, filer, channelID, 1)
	handled, err := handler.Handle(&slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.ReactionAddedEvent{
				Reaction: closedReaction,
				Item: slackevents.Item{
					Channel:   channelID,
					Timestamp: threadTS,
				},
			},
		},
	}, logrus.NewEntry(logrus.StandardLogger()))
	if err != nil {
		t.Fatalf("did not expect error: %v", err)
	}
	if !handled {
		t.Fatalf("expected reaction to be handled")
	}
	if len(filer.closeCalls) != 1 {
		t.Fatalf("expected exactly one jira close call, got %d", len(filer.closeCalls))
	}
	if diff := cmp.Diff(ciJira.CloseIssueRequest{IssueKey: "DPTP-99", Resolution: ciJira.ResolutionDone}, filer.closeCalls[0]); diff != "" {
		t.Fatalf("unexpected close issue call: %s", diff)
	}
}

func TestCloseSupportRequestIgnoresReplyReaction(t *testing.T) {
	channelID := "C123"
	rootTS := "100.100"
	replyTS := "100.101"
	client := &fakeClient{
		repliesByTS: map[string][]slack.Message{
			rootTS: {
				{Msg: slack.Msg{Timestamp: rootTS}},
				{Msg: slack.Msg{Text: fmt.Sprintf("%s <https://issues.redhat.com/browse/DPTP-42|DPTP-42>", supportRequestPrefix)}},
			},
		},
		historyByTS: map[string]*slack.GetConversationHistoryResponse{
			replyTS: {
				Messages: []slack.Message{
					{Msg: slack.Msg{Timestamp: replyTS, ThreadTimestamp: rootTS}},
				},
			},
		},
	}
	filer := &fakeFiler{closeResult: true}
	handler := Handler(client, filer, channelID, 5)
	handled, err := handler.Handle(&slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.ReactionAddedEvent{
				Reaction: closedReaction,
				Item: slackevents.Item{
					Channel:   channelID,
					Timestamp: replyTS,
				},
			},
		},
	}, logrus.NewEntry(logrus.StandardLogger()))
	if err != nil {
		t.Fatalf("did not expect error: %v", err)
	}
	if handled {
		t.Fatalf("expected reply reaction not to be handled")
	}
	if len(filer.closeCalls) != 0 {
		t.Fatalf("expected zero jira close calls, got %d", len(filer.closeCalls))
	}
}

func TestCloseSupportRequestIgnoresShipHelpRootReaction(t *testing.T) {
	channelID := "C123"
	rootTS := "100.100"
	client := &fakeClient{
		repliesByTS: map[string][]slack.Message{
			rootTS: {
				{Msg: slack.Msg{Timestamp: rootTS, Text: "@ship-help please assist"}},
				{Msg: slack.Msg{Text: fmt.Sprintf("%s <https://issues.redhat.com/browse/DPTP-42|DPTP-42>", supportRequestPrefix)}},
			},
		},
		historyByTS: map[string]*slack.GetConversationHistoryResponse{
			rootTS: {
				Messages: []slack.Message{
					{Msg: slack.Msg{Timestamp: rootTS, Text: "@ship-help please assist"}},
				},
			},
		},
	}
	filer := &fakeFiler{closeResult: true}
	handler := Handler(client, filer, channelID, 5)
	handled, err := handler.Handle(&slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.ReactionAddedEvent{
				Reaction: closedReaction,
				Item: slackevents.Item{
					Channel:   channelID,
					Timestamp: rootTS,
				},
			},
		},
	}, logrus.NewEntry(logrus.StandardLogger()))
	if err != nil {
		t.Fatalf("did not expect error: %v", err)
	}
	if handled {
		t.Fatalf("expected @ship-help root reaction not to be handled")
	}
	if len(filer.closeCalls) != 0 {
		t.Fatalf("expected zero jira close calls, got %d", len(filer.closeCalls))
	}
}

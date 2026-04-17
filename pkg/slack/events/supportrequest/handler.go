package supportrequest

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/openshift/ci-tools/pkg/jira"
	"github.com/openshift/ci-tools/pkg/slack/events"
)

const (
	closedReaction         = "closed"
	notApplicableReaction  = "not-applicable"
	shipHelpPrefix         = "@ship-help"
	supportRequestPrefix   = "Forum support request ticket:"
	defaultDateFormat      = "2006-01-02 15:04:05"
	issuesRedHatBrowseBase = "https://issues.redhat.com/browse/"
	defaultRepliesPageSize = 200
	maxSlackRetryAttempts  = 5
	initialSlackBackoff    = 500 * time.Millisecond
	maxSlackBackoff        = 8 * time.Second
)

var jiraKeyRegex = regexp.MustCompile(`\b([A-Z]+-\d+)\b`)

type messageClient interface {
	PostMessage(channelID string, options ...slack.MsgOption) (string, string, error)
	GetConversationReplies(params *slack.GetConversationRepliesParameters) (msgs []slack.Message, hasMore bool, nextCursor string, err error)
	GetConversationHistory(params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error)
	GetPermalink(params *slack.PermalinkParameters) (string, error)
}

type lockClient interface {
	Acquire(threadTS string) (bool, error)
	MarkProcessed(threadTS, issueKey string) error
	GetProcessedIssueKey(threadTS string) (string, bool, error)
	Release(threadTS string) error
}

type noopLockClient struct{}

func (noopLockClient) Acquire(threadTS string) (bool, error)         { return true, nil }
func (noopLockClient) MarkProcessed(threadTS, issueKey string) error { return nil }
func (noopLockClient) GetProcessedIssueKey(threadTS string) (string, bool, error) {
	return "", false, nil
}
func (noopLockClient) Release(threadTS string) error { return nil }

type processedThreadCache struct {
	ttl   time.Duration
	now   func() time.Time
	mutex sync.Mutex
	items map[string]time.Time
}

func newProcessedThreadCache(ttl time.Duration) *processedThreadCache {
	return &processedThreadCache{
		ttl:   ttl,
		now:   time.Now,
		items: map[string]time.Time{},
	}
}

func (c *processedThreadCache) HasFresh(threadTS string) bool {
	if c == nil {
		return false
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()
	expiresAt, found := c.items[threadTS]
	if !found {
		return false
	}
	now := c.now()
	if now.After(expiresAt) {
		delete(c.items, threadTS)
		return false
	}
	return true
}

func (c *processedThreadCache) MarkProcessed(threadTS string) {
	if c == nil {
		return
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.items[threadTS] = c.now().Add(c.ttl)
}

// Handler creates Jira support requests for long threads in a configured channel
// and closes the corresponding Jira issue when :closed: is added to the main thread.
func Handler(client messageClient, filer jira.IssueFiler, channelID string, threadMessageThreshold int) events.PartialHandler {
	return HandlerWithLock(client, filer, channelID, threadMessageThreshold, noopLockClient{})
}

// HandlerWithLock creates a handler with a cross-replica lock implementation.
func HandlerWithLock(client messageClient, filer jira.IssueFiler, channelID string, threadMessageThreshold int, locker lockClient) events.PartialHandler {
	// Prevents concurrent Jira creation for the same thread when
	// multiple replies arrive at nearly the same time.
	var inflight sync.Map
	processedCache := newProcessedThreadCache(24 * time.Hour)

	return events.PartialHandlerFunc("supportrequest", func(callback *slackevents.EventsAPIEvent, logger *logrus.Entry) (bool, error) {
		if channelID == "" {
			return false, nil
		}
		if callback.Type != slackevents.CallbackEvent {
			return false, nil
		}

		switch event := callback.InnerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			return handleMessage(event, client, filer, channelID, threadMessageThreshold, &inflight, locker, processedCache, logger)
		case *slackevents.ReactionAddedEvent:
			return handleReactionAdded(event, client, filer, channelID, locker, logger)
		default:
			return false, nil
		}
	})
}

func handleMessage(event *slackevents.MessageEvent, client messageClient, filer jira.IssueFiler, channelID string, threadMessageThreshold int, inflight *sync.Map, locker lockClient, processedCache *processedThreadCache, logger *logrus.Entry) (bool, error) {
	if event.Channel != channelID {
		logger.WithField("channel", event.Channel).Debug("supportrequest: skip message from non-configured channel")
		return false, nil
	}
	if event.ChannelType != "channel" {
		logger.WithField("channel_type", event.ChannelType).Debug("supportrequest: skip non-channel message event")
		return false, nil
	}
	if event.SubType != "" {
		logger.WithField("subtype", event.SubType).Debug("supportrequest: skip message subtype event")
		return false, nil
	}
	if event.ThreadTimeStamp == "" {
		logger.Debug("supportrequest: skip top-level message (not a thread reply)")
		return false, nil
	}
	if event.BotID != "" || event.User == "" {
		logger.WithFields(logrus.Fields{"bot_id": event.BotID, "user": event.User}).Debug("supportrequest: skip bot/system message")
		return false, nil
	}
	if processedCache.HasFresh(event.ThreadTimeStamp) {
		logger.WithField("thread_ts", event.ThreadTimeStamp).Debug("supportrequest: skip thread already processed (cache hit)")
		return false, nil
	}

	if _, loaded := inflight.LoadOrStore(event.ThreadTimeStamp, struct{}{}); loaded {
		logger.WithField("thread_ts", event.ThreadTimeStamp).Debug("supportrequest: skip concurrent in-flight processing for thread")
		return false, nil
	}
	defer inflight.Delete(event.ThreadTimeStamp)

	eligible, alreadyProcessed, reason, err := isEligibleSupportThread(client, channelID, event.ThreadTimeStamp, threadMessageThreshold)
	if err != nil {
		return true, err
	}
	if alreadyProcessed {
		processedCache.MarkProcessed(event.ThreadTimeStamp)
		logger.WithField("thread_ts", event.ThreadTimeStamp).Debug("supportrequest: thread has existing Jira marker, caching as processed")
	}
	if !eligible {
		logger.WithFields(logrus.Fields{"thread_ts": event.ThreadTimeStamp, "reason": reason}).Debug("supportrequest: thread not eligible for Jira creation")
		return false, nil
	}

	acquired, err := locker.Acquire(event.ThreadTimeStamp)
	if err != nil {
		return true, err
	}
	if !acquired {
		logger.WithField("thread_ts", event.ThreadTimeStamp).Debug("supportrequest: lock not acquired (already processing/processed)")
		return false, nil
	}
	shouldReleaseLock := true
	defer func() {
		if !shouldReleaseLock {
			return
		}
		if err := locker.Release(event.ThreadTimeStamp); err != nil {
			logger.WithError(err).Warn("Failed to release support request thread lock")
		}
	}()

	// Re-check under lock so only one replica can proceed to create.
	eligible, alreadyProcessed, reason, err = isEligibleSupportThread(client, channelID, event.ThreadTimeStamp, threadMessageThreshold)
	if err != nil {
		return true, err
	}
	if alreadyProcessed {
		processedCache.MarkProcessed(event.ThreadTimeStamp)
		logger.WithField("thread_ts", event.ThreadTimeStamp).Debug("supportrequest: thread already processed during lock re-check")
	}
	if !eligible {
		logger.WithFields(logrus.Fields{"thread_ts": event.ThreadTimeStamp, "reason": reason}).Debug("supportrequest: thread no longer eligible after lock re-check")
		return false, nil
	}

	permalink, err := getPermalinkWithRetry(client, &slack.PermalinkParameters{Channel: channelID, Ts: event.ThreadTimeStamp})
	if err != nil {
		logger.WithError(err).Warn("Failed to get Slack permalink for support request thread")
		permalink = ""
	}

	description := "Support request created from Slack thread."
	if permalink != "" {
		description = fmt.Sprintf("Support request created from Slack thread: %s", permalink)
	}
	title := fmt.Sprintf("support request on %s", time.Now().UTC().Format(defaultDateFormat))
	issue, err := filer.FileIssue(jira.IssueTypeTask, title, description, event.User, logger)
	if err != nil {
		_ = postMessageWithRetry(client, channelID, slack.MsgOptionText("Failed to create corresponding support request in Jira.", false), slack.MsgOptionTS(event.ThreadTimeStamp))
		return true, err
	}
	// Persist thread->issue mapping so close handling does not depend only
	// on parsing the Slack thread marker message.
	shouldReleaseLock = false
	if err := locker.MarkProcessed(event.ThreadTimeStamp, issue.Key); err != nil {
		return true, err
	}
	processedCache.MarkProcessed(event.ThreadTimeStamp)
	if err := filer.SetIssueStatus(issue.Key, jira.StatusInProgress, logger); err != nil {
		logger.WithError(err).WithField("issue", issue.Key).Warn("Failed to transition support request to In Progress")
	}

	issueURL := fmt.Sprintf("%s%s", issuesRedHatBrowseBase, issue.Key)
	postErr := postMessageWithRetry(client, channelID, slack.MsgOptionText(
		fmt.Sprintf(
			"%s <%s|%s>\nThis ticket was created automatically after this thread exceeded the threshold of %d messages. No user action is needed and conversation can continue in this forum thread.",
			supportRequestPrefix, issueURL, issue.Key, threadMessageThreshold,
		),
		false,
	), slack.MsgOptionTS(event.ThreadTimeStamp))
	if postErr != nil {
		logger.WithError(postErr).Warn("Failed to post support request Jira link in Slack thread")
	}

	return true, nil
}

func handleReactionAdded(event *slackevents.ReactionAddedEvent, client messageClient, filer jira.IssueFiler, channelID string, locker lockClient, logger *logrus.Entry) (bool, error) {
	if event.Item.Channel != channelID {
		logger.WithField("channel", event.Item.Channel).Debug("supportrequest: skip reaction from non-configured channel")
		return false, nil
	}
	if event.Reaction != closedReaction {
		logger.WithField("reaction", event.Reaction).Debug("supportrequest: skip non-closed reaction")
		return false, nil
	}

	threadTS, rootText, rootMessage, err := rootThreadTSForReaction(client, channelID, event.Item.Timestamp)
	if err != nil {
		return true, err
	}
	if !rootMessage {
		logger.WithField("message_ts", event.Item.Timestamp).Debug("supportrequest: skip closed reaction on non-root message")
		return false, nil
	}
	if rootStartsWithShipHelp(rootText) {
		logger.WithField("thread_ts", threadTS).Debug("supportrequest: skip close handling for @ship-help thread")
		return false, nil
	}

	if issueKey, found, err := locker.GetProcessedIssueKey(threadTS); err != nil {
		return true, err
	} else if found {
		logger.WithFields(logrus.Fields{"thread_ts": threadTS, "issue_key": issueKey}).Debug("supportrequest: closing Jira via lock mapping")
		return closeIssueAndNotify(client, filer, channelID, threadTS, issueKey, logger)
	}

	replies, rootMessage, err := getAllRepliesInThread(client, channelID, threadTS, defaultRepliesPageSize)
	if err != nil {
		return true, err
	}
	if !rootMessage {
		logger.WithField("thread_ts", threadTS).Debug("supportrequest: skip close handling, thread replies not rooted")
		return false, nil
	}
	issueKey, found := supportRequestIssueKeyFromReplies(replies)
	if !found {
		logger.WithField("thread_ts", threadTS).Debug("supportrequest: skip close handling, Jira marker not found in thread")
		return false, nil
	}

	logger.WithFields(logrus.Fields{"thread_ts": threadTS, "issue_key": issueKey}).Debug("supportrequest: closing Jira via thread marker")
	return closeIssueAndNotify(client, filer, channelID, threadTS, issueKey, logger)
}

func rootThreadTSForReaction(client messageClient, channelID, messageTS string) (threadTS string, rootText string, rootMessage bool, err error) {
	history, err := getConversationHistoryWithRetry(client, &slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Inclusive: true,
		Latest:    messageTS,
		Oldest:    messageTS,
		Limit:     1,
	})
	if err != nil {
		return "", "", false, err
	}
	if history == nil || len(history.Messages) == 0 {
		return "", "", false, nil
	}
	message := history.Messages[0]
	if message.ThreadTimestamp != "" && message.ThreadTimestamp != message.Timestamp {
		return "", "", false, nil
	}
	if message.Timestamp == "" {
		return "", "", false, nil
	}
	return message.Timestamp, message.Text, true, nil
}

func closeIssueAndNotify(client messageClient, filer jira.IssueFiler, channelID, threadTS, issueKey string, logger *logrus.Entry) (bool, error) {

	closed, err := filer.CloseIssue(issueKey, jira.ResolutionDone, logger)
	if err != nil {
		_ = postMessageWithRetry(client, channelID, slack.MsgOptionText(fmt.Sprintf("Failed to close support request %s.", issueKey), false), slack.MsgOptionTS(threadTS))
		return true, err
	}
	if !closed {
		_ = postMessageWithRetry(client, channelID, slack.MsgOptionText(fmt.Sprintf("Could not close support request %s because no valid closing transition was found.", issueKey), false), slack.MsgOptionTS(threadTS))
		return true, nil
	}

	issueURL := fmt.Sprintf("%s%s", issuesRedHatBrowseBase, issueKey)
	_ = postMessageWithRetry(client, channelID, slack.MsgOptionText(fmt.Sprintf("Closed corresponding support request: <%s|%s>", issueURL, issueKey), false), slack.MsgOptionTS(threadTS))
	return true, nil
}

func isEligibleSupportThread(client messageClient, channelID, threadTS string, threshold int) (eligible, alreadyProcessed bool, reason string, err error) {
	replies, rootMessage, err := getAllRepliesInThread(client, channelID, threadTS, defaultRepliesPageSize)
	if err != nil {
		return false, false, "replies_fetch_error", err
	}
	if !rootMessage || humanMessageCount(replies) <= threshold {
		if !rootMessage {
			return false, false, "not_root_thread", nil
		}
		return false, false, "human_threshold_not_reached", nil
	}
	if rootStartsWithShipHelp(replies[0].Text) {
		return false, false, "root_starts_with_ship_help", nil
	}
	if rootHasBlockingReaction(replies) {
		return false, false, "root_has_blocking_reaction", nil
	}
	if _, found := supportRequestIssueKeyFromReplies(replies); found {
		return false, true, "jira_marker_already_present", nil
	}
	return true, false, "eligible", nil
}

func rootHasBlockingReaction(replies []slack.Message) bool {
	if len(replies) == 0 {
		return false
	}
	for _, reaction := range replies[0].Reactions {
		if isBlockingReaction(reaction.Name) {
			return true
		}
	}
	return false
}

func isBlockingReaction(name string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "_", "-"))
	return normalized == closedReaction || normalized == notApplicableReaction
}

func humanMessageCount(messages []slack.Message) int {
	count := 0
	for _, message := range messages {
		if isHumanMessage(message) {
			count++
		}
	}
	return count
}

func isHumanMessage(message slack.Message) bool {
	if message.BotID != "" {
		return false
	}
	// Slack bot messages carry bot_message subtype.
	return message.SubType != "bot_message"
}

func rootStartsWithShipHelp(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), shipHelpPrefix)
}

func getAllRepliesInThread(client messageClient, channelID, threadTS string, pageSize int) (replies []slack.Message, rootMessage bool, err error) {
	if pageSize <= 0 {
		pageSize = defaultRepliesPageSize
	}
	cursor := ""
	firstPage := true
	allReplies := make([]slack.Message, 0, pageSize)
	for {
		pageReplies, hasMore, nextCursor, err := getConversationRepliesWithRetry(client, &slack.GetConversationRepliesParameters{
			ChannelID: channelID,
			Timestamp: threadTS,
			Inclusive: true,
			Limit:     pageSize,
			Cursor:    cursor,
		})
		if err != nil {
			return nil, false, err
		}
		if len(pageReplies) == 0 {
			return nil, false, nil
		}
		if firstPage {
			// We query this endpoint by threadTS (and reactions are
			// pre-validated against root via conversations.history),
			// so treat the first page as rooted thread context.
			rootMessage = true
			firstPage = false
		}
		allReplies = append(allReplies, pageReplies...)
		if !hasMore {
			break
		}
		cursor = nextCursor
	}
	return allReplies, rootMessage, nil
}

func supportRequestIssueKeyFromReplies(replies []slack.Message) (string, bool) {
	for _, reply := range replies {
		if !strings.Contains(reply.Text, supportRequestPrefix) {
			continue
		}
		matches := jiraKeyRegex.FindStringSubmatch(reply.Text)
		if len(matches) >= 2 {
			return matches[1], true
		}
	}
	return "", false
}

func getConversationRepliesWithRetry(client messageClient, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	var (
		msgs       []slack.Message
		hasMore    bool
		nextCursor string
	)
	err := withSlackRetry(func() error {
		var err error
		msgs, hasMore, nextCursor, err = client.GetConversationReplies(params)
		return err
	})
	return msgs, hasMore, nextCursor, err
}

func getConversationHistoryWithRetry(client messageClient, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	var history *slack.GetConversationHistoryResponse
	err := withSlackRetry(func() error {
		var err error
		history, err = client.GetConversationHistory(params)
		return err
	})
	return history, err
}

func getPermalinkWithRetry(client messageClient, params *slack.PermalinkParameters) (string, error) {
	var permalink string
	err := withSlackRetry(func() error {
		var err error
		permalink, err = client.GetPermalink(params)
		return err
	})
	return permalink, err
}

func postMessageWithRetry(client messageClient, channelID string, options ...slack.MsgOption) error {
	err := withSlackRetry(func() error {
		var err error
		_, _, err = client.PostMessage(channelID, options...)
		return err
	})
	return err
}

func withSlackRetry(op func() error) error {
	var lastErr error
	for attempt := 0; attempt < maxSlackRetryAttempts; attempt++ {
		if err := op(); err != nil {
			lastErr = err
			var rateLimitErr *slack.RateLimitedError
			if errors.As(err, &rateLimitErr) {
				time.Sleep(rateLimitErr.RetryAfter)
				continue
			}
			if !isRetryableSlackError(err) {
				return err
			}
			time.Sleep(backoffForAttempt(attempt))
			continue
		}
		return nil
	}
	return fmt.Errorf("slack request failed after %d retries: %w", maxSlackRetryAttempts, lastErr)
}

func isRetryableSlackError(err error) bool {
	type retryable interface {
		Retryable() bool
	}
	var retryableErr retryable
	if errors.As(err, &retryableErr) {
		return retryableErr.Retryable()
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return false
}

func backoffForAttempt(attempt int) time.Duration {
	backoff := initialSlackBackoff << attempt
	if backoff > maxSlackBackoff {
		return maxSlackBackoff
	}
	if backoff <= 0 {
		return maxSlackBackoff
	}
	return backoff
}

package jira

import (
	"errors"
	"testing"

	"github.com/andygrunwald/go-jira"
	"github.com/sirupsen/logrus"
)

// IssueRequest describes a client call to file an issue
type IssueRequest struct {
	IssueType, Title, Description, Reporter, ActivityType string
}

// CloseIssueRequest describes a client call to close an issue.
type CloseIssueRequest struct {
	IssueKey, Resolution string
}

// SetIssueStatusRequest describes a client call to set issue status.
type SetIssueStatusRequest struct {
	IssueKey, Status string
}

// IssueResponse describes a client response for filing an issue
type IssueResponse struct {
	Issue *jira.Issue
	Error error
}

// CloseIssueResponse describes a client response for closing an issue.
type CloseIssueResponse struct {
	Closed bool
	Error  error
}

// SetIssueStatusResponse describes a client response for setting status.
type SetIssueStatusResponse struct {
	Error error
}

// Fake is an injectable IssueFiler
type Fake struct {
	behavior            map[IssueRequest]IssueResponse
	closeBehavior       map[CloseIssueRequest]CloseIssueResponse
	statusBehavior      map[SetIssueStatusRequest]SetIssueStatusResponse
	unwanted            []IssueRequest
	unwantedCloseCalls  []CloseIssueRequest
	unwantedStatusCalls []SetIssueStatusRequest
}

// SetCloseBehavior configures expected CloseIssue calls and responses.
func (f *Fake) SetCloseBehavior(behavior map[CloseIssueRequest]CloseIssueResponse) {
	f.closeBehavior = make(map[CloseIssueRequest]CloseIssueResponse, len(behavior))
	for request, response := range behavior {
		f.closeBehavior[request] = response
	}
}

// SetStatusBehavior configures expected SetIssueStatus calls and responses.
func (f *Fake) SetStatusBehavior(behavior map[SetIssueStatusRequest]SetIssueStatusResponse) {
	f.statusBehavior = make(map[SetIssueStatusRequest]SetIssueStatusResponse, len(behavior))
	for request, response := range behavior {
		f.statusBehavior[request] = response
	}
}

// FileIssue files the issue using injected behavior
func (f *Fake) FileIssue(issueType, title, description, reporter, activityType string, logger *logrus.Entry) (*jira.Issue, error) {
	request := IssueRequest{
		IssueType:    issueType,
		Title:        title,
		Description:  description,
		Reporter:     reporter,
		ActivityType: activityType,
	}
	response, registered := f.behavior[request]
	if !registered {
		f.unwanted = append(f.unwanted, request)
		return nil, errors.New("no such issue request behavior in fake")
	}
	delete(f.behavior, request)
	return response.Issue, response.Error
}

// CloseIssue closes the issue using injected behavior.
func (f *Fake) CloseIssue(issueKey, resolution string, logger *logrus.Entry) (bool, error) {
	request := CloseIssueRequest{
		IssueKey:   issueKey,
		Resolution: resolution,
	}
	response, registered := f.closeBehavior[request]
	if !registered {
		f.unwantedCloseCalls = append(f.unwantedCloseCalls, request)
		return false, errors.New("no such close issue behavior in fake")
	}
	delete(f.closeBehavior, request)
	return response.Closed, response.Error
}

// SetIssueStatus sets issue status using injected behavior.
func (f *Fake) SetIssueStatus(issueKey, status string, logger *logrus.Entry) error {
	request := SetIssueStatusRequest{
		IssueKey: issueKey,
		Status:   status,
	}
	response, registered := f.statusBehavior[request]
	if !registered {
		f.unwantedStatusCalls = append(f.unwantedStatusCalls, request)
		return errors.New("no such set issue status behavior in fake")
	}
	delete(f.statusBehavior, request)
	return response.Error
}

// Validate ensures that all expected client calls happened
func (f *Fake) Validate(t *testing.T) {
	for request := range f.behavior {
		t.Errorf("fake issue filer did not get request: %v", request)
	}
	for request := range f.closeBehavior {
		t.Errorf("fake issue filer did not get close request: %v", request)
	}
	for request := range f.statusBehavior {
		t.Errorf("fake issue filer did not get set-status request: %v", request)
	}
	for _, request := range f.unwanted {
		t.Errorf("fake issue filer got unwanted request: %v", request)
	}
	for _, request := range f.unwantedCloseCalls {
		t.Errorf("fake issue filer got unwanted close request: %v", request)
	}
	for _, request := range f.unwantedStatusCalls {
		t.Errorf("fake issue filer got unwanted set-status request: %v", request)
	}
}

var _ IssueFiler = &Fake{}

// NewFake creates a new fake filer with the injected behavior
func NewFake(calls map[IssueRequest]IssueResponse) *Fake {
	return &Fake{
		behavior:       calls,
		closeBehavior:  map[CloseIssueRequest]CloseIssueResponse{},
		statusBehavior: map[SetIssueStatusRequest]SetIssueStatusResponse{},
	}
}

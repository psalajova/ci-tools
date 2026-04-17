package supportrequest

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlruntimefake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testNamespace = "ci"

func newTestLockClient(t *testing.T, now func() time.Time) *configMapLockClient {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add corev1 scheme: %v", err)
	}
	client := ctrlruntimefake.NewClientBuilder().WithScheme(scheme).Build()
	return &configMapLockClient{
		client:    client,
		namespace: testNamespace,
		now:       now,
	}
}

func TestConfigMapLockAcquireRelease(t *testing.T) {
	now := time.Date(2026, time.April, 17, 10, 0, 0, 0, time.UTC)
	locker := newTestLockClient(t, func() time.Time { return now })

	acquired, err := locker.Acquire("100.100")
	if err != nil {
		t.Fatalf("unexpected acquire error: %v", err)
	}
	if !acquired {
		t.Fatalf("expected first acquire to succeed")
	}

	acquired, err = locker.Acquire("100.100")
	if err != nil {
		t.Fatalf("unexpected second acquire error: %v", err)
	}
	if acquired {
		t.Fatalf("expected second acquire to fail while lock is held")
	}

	if err := locker.Release("100.100"); err != nil {
		t.Fatalf("unexpected release error: %v", err)
	}

	acquired, err = locker.Acquire("100.100")
	if err != nil {
		t.Fatalf("unexpected acquire-after-release error: %v", err)
	}
	if !acquired {
		t.Fatalf("expected acquire after release to succeed")
	}
}

func TestConfigMapLockProcessedNeverReacquires(t *testing.T) {
	now := time.Date(2026, time.April, 17, 10, 0, 0, 0, time.UTC)
	locker := newTestLockClient(t, func() time.Time { return now })

	acquired, err := locker.Acquire("200.200")
	if err != nil || !acquired {
		t.Fatalf("expected initial acquire success, got acquired=%t err=%v", acquired, err)
	}
	if err := locker.MarkProcessed("200.200", "DPTP-200"); err != nil {
		t.Fatalf("unexpected mark processed error: %v", err)
	}

	now = now.Add(48 * time.Hour)
	acquired, err = locker.Acquire("200.200")
	if err != nil {
		t.Fatalf("unexpected acquire error for processed lock: %v", err)
	}
	if acquired {
		t.Fatalf("expected processed lock to block reacquire even after TTL")
	}
}

func TestConfigMapLockExpiredProcessingCanReacquire(t *testing.T) {
	now := time.Date(2026, time.April, 17, 10, 0, 0, 0, time.UTC)
	locker := newTestLockClient(t, func() time.Time { return now })

	acquired, err := locker.Acquire("300.300")
	if err != nil || !acquired {
		t.Fatalf("expected initial acquire success, got acquired=%t err=%v", acquired, err)
	}

	now = now.Add((time.Duration(lockTTLSeconds) * time.Second) + time.Second)
	acquired, err = locker.Acquire("300.300")
	if err != nil {
		t.Fatalf("unexpected acquire error after TTL: %v", err)
	}
	if !acquired {
		t.Fatalf("expected reacquire after processing TTL expiry")
	}
}

func TestConfigMapLockReleaseDoesNotDeleteProcessed(t *testing.T) {
	now := time.Date(2026, time.April, 17, 10, 0, 0, 0, time.UTC)
	locker := newTestLockClient(t, func() time.Time { return now })

	acquired, err := locker.Acquire("400.400")
	if err != nil || !acquired {
		t.Fatalf("expected initial acquire success, got acquired=%t err=%v", acquired, err)
	}
	if err := locker.MarkProcessed("400.400", "DPTP-400"); err != nil {
		t.Fatalf("unexpected mark processed error: %v", err)
	}
	if err := locker.Release("400.400"); err != nil {
		t.Fatalf("unexpected release error: %v", err)
	}

	now = now.Add(24 * time.Hour)
	acquired, err = locker.Acquire("400.400")
	if err != nil {
		t.Fatalf("unexpected acquire error for processed lock: %v", err)
	}
	if acquired {
		t.Fatalf("expected processed lock to remain after release")
	}
}

func TestConfigMapLockGetProcessedIssueKey(t *testing.T) {
	now := time.Date(2026, time.April, 17, 10, 0, 0, 0, time.UTC)
	locker := newTestLockClient(t, func() time.Time { return now })

	acquired, err := locker.Acquire("500.500")
	if err != nil || !acquired {
		t.Fatalf("expected initial acquire success, got acquired=%t err=%v", acquired, err)
	}
	if err := locker.MarkProcessed("500.500", "DPTP-500"); err != nil {
		t.Fatalf("unexpected mark processed error: %v", err)
	}

	issueKey, found, err := locker.GetProcessedIssueKey("500.500")
	if err != nil {
		t.Fatalf("unexpected get processed issue key error: %v", err)
	}
	if !found {
		t.Fatalf("expected processed issue key to be found")
	}
	if issueKey != "DPTP-500" {
		t.Fatalf("expected issue key DPTP-500, got %s", issueKey)
	}
}

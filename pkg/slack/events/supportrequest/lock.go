package supportrequest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	threadLockMapName         = "slack-supportrequest-locks"
	maxLockRetries            = 5
	lockTTLSeconds            = int64((30 * time.Minute) / time.Second)
	lockStateProcessingPrefix = "processing:"
	lockStateProcessedPrefix  = "processed:"
)

type configMapLockClient struct {
	client    ctrlruntimeclient.Client
	namespace string
	now       func() time.Time
}

// NewConfigMapLockClient returns a cross-replica lock based on ConfigMap create/delete.
func NewConfigMapLockClient(client ctrlruntimeclient.Client, namespace string) lockClient {
	return &configMapLockClient{
		client:    client,
		namespace: namespace,
		now:       time.Now,
	}
}

func (c *configMapLockClient) Acquire(threadTS string) (bool, error) {
	key := lockNameForThread(threadTS)
	for i := 0; i < maxLockRetries; i++ {
		lockMap, err := c.getOrCreateLockMap()
		if err != nil {
			return false, err
		}
		if lockMap.Data == nil {
			lockMap.Data = map[string]string{}
		}
		if state, exists := lockMap.Data[key]; exists {
			if _, ok := parseProcessedState(state); ok {
				return false, nil
			}
			if acquiredAt, ok := parseProcessingState(state); ok {
				if c.now().UTC().Unix()-acquiredAt <= lockTTLSeconds {
					return false, nil
				}
			} else {
				// Unknown state, keep lock as held to avoid duplicate work.
				return false, nil
			}
		}
		lockMap.Data[key] = processingState(c.now().UTC().Unix())
		if err := c.client.Update(context.TODO(), lockMap); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return false, err
		}
		return true, nil
	}
	return false, fmt.Errorf("failed to acquire lock for %s after retries", threadTS)
}

func (c *configMapLockClient) MarkProcessed(threadTS, issueKey string) error {
	key := lockNameForThread(threadTS)
	for i := 0; i < maxLockRetries; i++ {
		lockMap := &corev1.ConfigMap{}
		if err := c.client.Get(context.TODO(), ctrlruntimeclient.ObjectKey{Namespace: c.namespace, Name: threadLockMapName}, lockMap); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if lockMap.Data == nil {
			lockMap.Data = map[string]string{}
		}
		lockMap.Data[key] = processedState(issueKey)
		if err := c.client.Update(context.TODO(), lockMap); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("failed to mark lock for %s as processed after retries", threadTS)
}

func (c *configMapLockClient) GetProcessedIssueKey(threadTS string) (string, bool, error) {
	key := lockNameForThread(threadTS)
	lockMap := &corev1.ConfigMap{}
	if err := c.client.Get(context.TODO(), ctrlruntimeclient.ObjectKey{Namespace: c.namespace, Name: threadLockMapName}, lockMap); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if lockMap.Data == nil {
		return "", false, nil
	}
	state, exists := lockMap.Data[key]
	if !exists {
		return "", false, nil
	}
	issueKey, ok := parseProcessedState(state)
	return issueKey, ok, nil
}

func (c *configMapLockClient) Release(threadTS string) error {
	key := lockNameForThread(threadTS)
	for i := 0; i < maxLockRetries; i++ {
		lockMap := &corev1.ConfigMap{}
		if err := c.client.Get(context.TODO(), ctrlruntimeclient.ObjectKey{Namespace: c.namespace, Name: threadLockMapName}, lockMap); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if lockMap.Data == nil {
			return nil
		}
		if _, exists := lockMap.Data[key]; !exists {
			return nil
		}
		if _, ok := parseProcessedState(lockMap.Data[key]); ok {
			return nil
		}
		delete(lockMap.Data, key)
		if err := c.client.Update(context.TODO(), lockMap); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("failed to release lock for %s after retries", threadTS)
}

func lockNameForThread(threadTS string) string {
	sum := sha256.Sum256([]byte(threadTS))
	return fmt.Sprintf("%x", sum[:8])
}

func processingState(unixSeconds int64) string {
	return fmt.Sprintf("%s%d", lockStateProcessingPrefix, unixSeconds)
}

func processedState(issueKey string) string {
	return fmt.Sprintf("%s%s", lockStateProcessedPrefix, issueKey)
}

func parseProcessingState(state string) (int64, bool) {
	if !strings.HasPrefix(state, lockStateProcessingPrefix) {
		return 0, false
	}
	value := strings.TrimPrefix(state, lockStateProcessingPrefix)
	ts, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return ts, true
}

func parseProcessedState(state string) (string, bool) {
	if !strings.HasPrefix(state, lockStateProcessedPrefix) {
		return "", false
	}
	issueKey := strings.TrimPrefix(state, lockStateProcessedPrefix)
	if issueKey == "" {
		return "", false
	}
	return issueKey, true
}

func (c *configMapLockClient) getOrCreateLockMap() (*corev1.ConfigMap, error) {
	lockMap := &corev1.ConfigMap{}
	key := ctrlruntimeclient.ObjectKey{Namespace: c.namespace, Name: threadLockMapName}
	if err := c.client.Get(context.TODO(), key, lockMap); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		toCreate := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: c.namespace,
				Name:      threadLockMapName,
				Labels: map[string]string{
					"app": "slack-bot-supportrequest-lock",
				},
			},
			Data: map[string]string{},
		}
		if err := c.client.Create(context.TODO(), toCreate); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		if err := c.client.Get(context.TODO(), key, lockMap); err != nil {
			return nil, err
		}
	}
	return lockMap, nil
}

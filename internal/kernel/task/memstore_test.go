package task

import (
	"context"
	"sort"
	"sync"
	"time"
)

// memStore 是后台任务 Store 端口的内存实现，仅供测试使用。
// 语义与 SQLite 适配器保持一致：租约令牌与到期条件作为原子守卫，
// 状态迁移拒绝非法转移并返回与生产一致的稳定错误。
type memStore struct {
	mu    sync.Mutex
	tasks map[string]*Task
}

func newMemStore() *memStore {
	return &memStore{tasks: make(map[string]*Task)}
}

func (m *memStore) key(appID, taskID string) string {
	return appID + "\x00" + taskID
}

func (m *memStore) copy(value Task) Task {
	value.Params = append([]byte(nil), value.Params...)
	if value.LeaseExpiresAt != nil {
		expires := *value.LeaseExpiresAt
		value.LeaseExpiresAt = &expires
	}
	return value
}

func (m *memStore) CreateTask(_ context.Context, value Task) error {
	if err := ValidateNewTask(value); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := m.key(value.AppID, value.TaskID)
	if _, exists := m.tasks[key]; exists {
		return ErrTaskExists
	}
	stored := m.copy(value)
	m.tasks[key] = &stored
	return nil
}

func (m *memStore) ClaimTask(_ context.Context, appID, taskID, leaseToken string, startedAt, leaseExpiresAt time.Time) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.tasks[m.key(appID, taskID)]
	if entry == nil {
		return Task{}, ErrInvalidTransition
	}
	if entry.Status != StatusQueued && entry.Status != StatusRetryScheduled {
		return Task{}, ErrInvalidTransition
	}
	if entry.LeaseToken != "" || entry.AvailableAt.After(startedAt) {
		return Task{}, ErrInvalidTransition
	}
	entry.Status = StatusRunning
	entry.LeaseToken = leaseToken
	entry.LeaseExpiresAt = &leaseExpiresAt
	entry.UpdatedAt = startedAt
	return m.copy(*entry), nil
}

// leaseFailure 区分“租约丢失”（任务仍为 running）与“状态非法”。
func (m *memStore) leaseFailure(entry *Task) error {
	if entry != nil && entry.Status == StatusRunning {
		return ErrLeaseLost
	}
	return ErrInvalidTransition
}

func (m *memStore) RenewTaskLease(_ context.Context, value Task, renewedAt, leaseExpiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.tasks[m.key(value.AppID, value.TaskID)]
	if entry == nil || entry.Status != StatusRunning || entry.LeaseToken != value.LeaseToken {
		return m.leaseFailure(entry)
	}
	if entry.LeaseExpiresAt == nil || entry.LeaseExpiresAt.Before(renewedAt) {
		return ErrLeaseLost
	}
	entry.LeaseExpiresAt = &leaseExpiresAt
	entry.UpdatedAt = renewedAt
	return nil
}

func (m *memStore) CompleteTask(_ context.Context, value Task, completedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.tasks[m.key(value.AppID, value.TaskID)]
	if entry == nil || entry.Status != StatusRunning || entry.LeaseToken != value.LeaseToken {
		return m.leaseFailure(entry)
	}
	if entry.LeaseExpiresAt == nil || entry.LeaseExpiresAt.Before(completedAt) {
		return ErrLeaseLost
	}
	entry.Status = StatusSucceeded
	entry.LeaseToken = ""
	entry.LeaseExpiresAt = nil
	entry.ErrorClass = ErrorClassNone
	entry.UpdatedAt = completedAt
	return nil
}

func (m *memStore) FailTask(_ context.Context, value Task, errorClass ErrorClass, completedAt time.Time) error {
	if err := validateTestFailureClass(errorClass); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.tasks[m.key(value.AppID, value.TaskID)]
	if entry == nil || entry.Status != StatusRunning || entry.LeaseToken != value.LeaseToken {
		return m.leaseFailure(entry)
	}
	if entry.LeaseExpiresAt == nil || entry.LeaseExpiresAt.Before(completedAt) {
		return ErrLeaseLost
	}
	entry.Status = StatusFailed
	entry.LeaseToken = ""
	entry.LeaseExpiresAt = nil
	entry.ErrorClass = errorClass
	entry.UpdatedAt = completedAt
	return nil
}

func (m *memStore) RetryTask(_ context.Context, value Task, nextAvailableAt, completedAt time.Time) error {
	if value.Status != StatusRunning || value.LeaseToken == "" ||
		!nextAvailableAt.After(completedAt) || value.Attempt >= value.MaxAttempts {
		return ErrInvalidTask
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.tasks[m.key(value.AppID, value.TaskID)]
	if entry == nil || entry.Status != StatusRunning || entry.LeaseToken != value.LeaseToken {
		return m.leaseFailure(entry)
	}
	if entry.LeaseExpiresAt == nil || entry.LeaseExpiresAt.Before(completedAt) {
		return ErrLeaseLost
	}
	entry.Status = StatusRetryScheduled
	entry.Attempt++
	entry.LeaseToken = ""
	entry.LeaseExpiresAt = nil
	entry.AvailableAt = nextAvailableAt
	entry.UpdatedAt = completedAt
	return nil
}

func (m *memStore) CancelQueuedTask(_ context.Context, appID, taskID string, completedAt time.Time) (Task, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.tasks[m.key(appID, taskID)]
	if entry == nil {
		return Task{}, false, ErrTaskNotFound
	}
	if entry.Status != StatusQueued && entry.Status != StatusRetryScheduled {
		return m.copy(*entry), false, nil
	}
	entry.Status = StatusCancelled
	entry.LeaseToken = ""
	entry.LeaseExpiresAt = nil
	entry.ErrorClass = ErrorClassCancelled
	entry.UpdatedAt = completedAt
	return m.copy(*entry), true, nil
}

func (m *memStore) CancelRunningTask(_ context.Context, value Task, completedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.tasks[m.key(value.AppID, value.TaskID)]
	if entry == nil || entry.Status != StatusRunning || entry.LeaseToken != value.LeaseToken {
		return m.leaseFailure(entry)
	}
	if entry.LeaseExpiresAt == nil || entry.LeaseExpiresAt.Before(completedAt) {
		return ErrLeaseLost
	}
	entry.Status = StatusCancelled
	entry.LeaseToken = ""
	entry.LeaseExpiresAt = nil
	entry.ErrorClass = ErrorClassCancelled
	entry.UpdatedAt = completedAt
	return nil
}

func (m *memStore) ListDueTasks(_ context.Context, now time.Time, limit int) ([]Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Task, 0)
	for _, entry := range m.tasks {
		if (entry.Status == StatusQueued || entry.Status == StatusRetryScheduled) &&
			entry.LeaseToken == "" && !entry.AvailableAt.After(now) {
			result = append(result, m.copy(*entry))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].AvailableAt.Equal(result[j].AvailableAt) {
			return result[i].AvailableAt.Before(result[j].AvailableAt)
		}
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		return result[i].TaskID < result[j].TaskID
	})
	return truncateTasks(result, limit), nil
}

func (m *memStore) ListDeadTasks(_ context.Context, now time.Time, limit int) ([]Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Task, 0)
	for _, entry := range m.tasks {
		if entry.Status == StatusRunning && entry.LeaseToken != "" &&
			entry.LeaseExpiresAt != nil && entry.LeaseExpiresAt.Before(now) {
			result = append(result, m.copy(*entry))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		left := result[i].LeaseExpiresAt
		right := result[j].LeaseExpiresAt
		if !left.Equal(*right) {
			return left.Before(*right)
		}
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		return result[i].TaskID < result[j].TaskID
	})
	return truncateTasks(result, limit), nil
}

func (m *memStore) RetryDeadTask(_ context.Context, value Task, nextAvailableAt, completedAt time.Time) error {
	if value.Status != StatusRunning || value.LeaseToken == "" ||
		!nextAvailableAt.After(completedAt) || value.Attempt >= value.MaxAttempts {
		return ErrInvalidTask
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.tasks[m.key(value.AppID, value.TaskID)]
	if entry == nil || entry.Status != StatusRunning || entry.LeaseToken != value.LeaseToken {
		return m.leaseFailure(entry)
	}
	if entry.LeaseExpiresAt == nil || !entry.LeaseExpiresAt.Before(completedAt) {
		return ErrLeaseLost
	}
	entry.Status = StatusRetryScheduled
	entry.Attempt++
	entry.LeaseToken = ""
	entry.LeaseExpiresAt = nil
	entry.AvailableAt = nextAvailableAt
	entry.UpdatedAt = completedAt
	return nil
}

func (m *memStore) FailDeadTask(_ context.Context, value Task, errorClass ErrorClass, completedAt time.Time) error {
	if err := validateTestFailureClass(errorClass); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.tasks[m.key(value.AppID, value.TaskID)]
	if entry == nil || entry.Status != StatusRunning || entry.LeaseToken != value.LeaseToken {
		return m.leaseFailure(entry)
	}
	if entry.LeaseExpiresAt == nil || !entry.LeaseExpiresAt.Before(completedAt) {
		return ErrLeaseLost
	}
	entry.Status = StatusFailed
	entry.LeaseToken = ""
	entry.LeaseExpiresAt = nil
	entry.ErrorClass = errorClass
	entry.UpdatedAt = completedAt
	return nil
}

func (m *memStore) GetTask(_ context.Context, appID, taskID string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.tasks[m.key(appID, taskID)]
	if entry == nil {
		return Task{}, ErrTaskNotFound
	}
	return m.copy(*entry), nil
}

func (m *memStore) ListTasks(_ context.Context, appID string, limit int) ([]Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Task, 0)
	for _, entry := range m.tasks {
		if entry.AppID == appID {
			result = append(result, m.copy(*entry))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		return result[i].TaskID < result[j].TaskID
	})
	return truncateTasks(result, limit), nil
}

func truncateTasks(values []Task, limit int) []Task {
	if limit > 0 && len(values) > limit {
		return values[:limit]
	}
	return values
}

// validateTestFailureClass 与存储适配器的失败分类校验保持一致。
func validateTestFailureClass(errorClass ErrorClass) error {
	switch errorClass {
	case ErrorClassRetryable, ErrorClassNonRetryable,
		ErrorClassDeadline, ErrorClassLeaseLost, ErrorClassCancelled:
		return nil
	default:
		return ErrInvalidTask
	}
}

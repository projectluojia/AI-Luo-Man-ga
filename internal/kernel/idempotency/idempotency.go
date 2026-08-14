package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
)

var (
	ErrInvalidRequest  = errors.New("invalid idempotency request")
	ErrKeyConflict     = errors.New("idempotency key was reused for a different request")
	ErrOutcomeUnknown  = errors.New("previous idempotent operation outcome is unknown")
	ErrPreviousFailure = errors.New("previous idempotent operation failed")
	ErrLeaseLost       = errors.New("idempotency execution lease was lost")
	ErrResultTooLarge  = errors.New("idempotency result exceeds storage limit")
	ErrRecordNotFound  = errors.New("idempotency record not found")
)

const (
	StatusExecuting = "executing"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"

	maxKeyBytes         = 128
	maxScopeBytes       = 256
	maxOwnerBytes       = 256
	maxResultBytes      = 256 << 10
	defaultLease        = 2 * time.Minute
	defaultRetention    = 7 * 24 * time.Hour
	defaultPollInterval = 20 * time.Millisecond
	cleanupTimeout      = 2 * time.Second
)

var (
	keyPattern   = id.StableMixedUncapped
	scopePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._:/-][A-Za-z0-9]+)*$`)
)

type Operation struct {
	AppID       string
	Scope       string
	Key         string
	Fingerprint string
	OwnerID     string
}

type Claim struct {
	Operation
	LeaseToken     string
	LeaseExpiresAt time.Time
}

type Record struct {
	Operation
	Status         string
	LeaseToken     string
	LeaseExpiresAt time.Time
	Result         []byte
	ErrorCode      string
	CreatedAt      time.Time
	CompletedAt    *time.Time
	ExpiresAt      *time.Time
}

type Store interface {
	BeginIdempotent(context.Context, Claim, time.Time) (Record, bool, error)
	GetIdempotent(context.Context, string, string, string) (Record, error)
	CompleteIdempotent(context.Context, Claim, string, []byte, string, time.Time, time.Time) error
}

type Manager struct {
	store        Store
	now          func() time.Time
	lease        time.Duration
	retention    time.Duration
	pollInterval time.Duration
}

func NewManager(store Store) *Manager {
	return &Manager{
		store:        store,
		now:          time.Now,
		lease:        defaultLease,
		retention:    defaultRetention,
		pollInterval: defaultPollInterval,
	}
}

func ValidateKey(value string) error {
	if len(value) == 0 || len(value) > maxKeyBytes || !keyPattern.MatchString(value) {
		return ErrInvalidRequest
	}
	return nil
}

func Fingerprint(parts ...[]byte) string {
	digest := sha256.New()
	var size [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		digest.Write(size[:])
		digest.Write(part)
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func (m *Manager) Execute(
	ctx context.Context,
	operation Operation,
	execute func(context.Context) ([]byte, error),
) ([]byte, bool, error) {
	if err := ValidateOperation(operation); err != nil {
		return nil, false, err
	}
	if execute == nil {
		return nil, false, ErrInvalidRequest
	}
	now := m.now().UTC()
	leaseExpiresAt := now.Add(m.lease)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(leaseExpiresAt) {
		leaseExpiresAt = deadline.UTC()
	}
	if !leaseExpiresAt.After(now) {
		return nil, false, context.DeadlineExceeded
	}
	claim := Claim{
		Operation:      operation,
		LeaseToken:     uuid.NewString(),
		LeaseExpiresAt: leaseExpiresAt,
	}
	record, claimed, err := m.store.BeginIdempotent(ctx, claim, now)
	if err != nil {
		return nil, false, err
	}
	if !claimed {
		return m.replayOrWait(ctx, operation, record)
	}

	result, executeErr := execute(ctx)
	if len(result) > maxResultBytes {
		executeErr = errors.Join(executeErr, ErrResultTooLarge)
		result = nil
	}
	status := StatusSucceeded
	errorCode := ""
	if executeErr != nil {
		status = StatusFailed
		errorCode = "operation_failed"
		result = nil
	}
	completedAt := m.now().UTC()
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	completeErr := m.store.CompleteIdempotent(cleanupContext, claim, status, result, errorCode, completedAt, completedAt.Add(m.retention))
	cancel()
	if completeErr != nil {
		return nil, false, errors.Join(executeErr, completeErr)
	}
	if executeErr != nil {
		return nil, false, executeErr
	}
	return append([]byte(nil), result...), false, nil
}

func (m *Manager) replayOrWait(ctx context.Context, operation Operation, record Record) ([]byte, bool, error) {
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	for {
		switch record.Status {
		case StatusSucceeded:
			return append([]byte(nil), record.Result...), true, nil
		case StatusFailed:
			return nil, true, ErrPreviousFailure
		case StatusExecuting:
			if !m.now().UTC().Before(record.LeaseExpiresAt) {
				return nil, true, ErrOutcomeUnknown
			}
		default:
			return nil, true, ErrOutcomeUnknown
		}
		select {
		case <-ctx.Done():
			return nil, true, ctx.Err()
		case <-ticker.C:
			var err error
			record, err = m.store.GetIdempotent(ctx, operation.AppID, operation.Scope, operation.Key)
			if err != nil {
				return nil, true, err
			}
			if record.Fingerprint != operation.Fingerprint {
				return nil, true, ErrKeyConflict
			}
		}
	}
}

func ValidateOperation(operation Operation) error {
	switch {
	case operation.AppID == "":
		return ErrInvalidRequest
	case len(operation.Scope) == 0 || len(operation.Scope) > maxScopeBytes || !scopePattern.MatchString(operation.Scope):
		return ErrInvalidRequest
	case ValidateKey(operation.Key) != nil:
		return ErrInvalidRequest
	case len(operation.Fingerprint) != sha256.Size*2:
		return ErrInvalidRequest
	case len(operation.OwnerID) == 0 || len(operation.OwnerID) > maxOwnerBytes:
		return ErrInvalidRequest
	default:
		return nil
	}
}

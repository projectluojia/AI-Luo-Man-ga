// Package academiccalendar 提供武大校历查询 Tool 的模型与存储端口。
package academiccalendar

import (
	"context"
	"errors"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalid        = errors.New("invalid academic calendar input")
	ErrQueryRequired  = errors.New("academic calendar query requires a date range")
	ErrDataIncomplete = contracts.ErrDataIncomplete
	ErrDataUntrusted  = contracts.ErrDataUntrusted
	ErrDataExpired    = contracts.ErrDataExpired
)

const (
	DataStateAuthoritativeFresh = "authoritative_fresh"
	MaxEvents                   = 5000
	MaxRevision                 = 128
	MaxSource                   = 128
	MaxEventID                  = 128
	MaxEventTitle               = 256
	MaxEventType                = 64
	MaxEventDescription         = 2048
	SourceWUDA                  = "zhihui-luojia"
	SourceDemo                  = "demo-fixture-not-zhihui-luojia"
)

type SnapshotMetadata struct {
	Revision      string    `json:"source_revision"`
	Source        string    `json:"source"`
	Authoritative bool      `json:"authoritative"`
	Complete      bool      `json:"complete"`
	ImportedAt    time.Time `json:"imported_at"`
	ValidUntil    time.Time `json:"valid_until"`
}

type DataStatus struct {
	State string `json:"state"`
	SnapshotMetadata
}

func (m SnapshotMetadata) Govern(now time.Time) (DataStatus, error) {
	if m.Revision == "" || m.Source == "" || !m.Complete || m.ImportedAt.IsZero() || m.ValidUntil.IsZero() || !m.ValidUntil.After(m.ImportedAt) || now.Before(m.ImportedAt) {
		return DataStatus{}, ErrDataIncomplete
	}
	if !m.Authoritative {
		return DataStatus{}, ErrDataUntrusted
	}
	if !now.Before(m.ValidUntil) {
		return DataStatus{}, ErrDataExpired
	}
	return DataStatus{State: DataStateAuthoritativeFresh, SnapshotMetadata: m}, nil
}

type Event struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	Type           string    `json:"type"`
	StartAt        time.Time `json:"start_at"`
	EndAt          time.Time `json:"end_at"`
	Description    string    `json:"description,omitempty"`
	SourceRevision string    `json:"source_revision"`
}

type QueryRequest struct {
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	Limit int       `json:"limit,omitempty"`
}

func (r *QueryRequest) NormalizeAndValidate() error {
	if r.From.IsZero() || r.To.IsZero() || !r.To.After(r.From) || r.To.Sub(r.From) > 366*24*time.Hour {
		return ErrQueryRequired
	}
	if r.Limit == 0 {
		r.Limit = 100
	}
	if r.Limit < 1 || r.Limit > MaxEvents {
		return ErrInvalid
	}
	return nil
}

type Snapshot struct {
	Metadata SnapshotMetadata
	Events   []Event
}

type Store interface {
	Search(context.Context, string, QueryRequest) (Snapshot, error)
}

// SnapshotWriter 由 Go 托管存储实现，用于经授权的数据接入或显式 demo 导入。
type SnapshotWriter interface {
	ReplaceSnapshot(context.Context, SnapshotInput) error
}

type SnapshotInput struct {
	AppID         string
	Revision      string
	Source        string
	Authoritative bool
	Complete      bool
	ImportedAt    time.Time
	ValidUntil    time.Time
	Events        []Event
}

func ValidateSnapshotInput(in SnapshotInput) error {
	if !validText(in.AppID, 128) || !validText(in.Revision, MaxRevision) || !validText(in.Source, MaxSource) || !in.Complete || in.ImportedAt.IsZero() || in.ValidUntil.IsZero() || !in.ValidUntil.After(in.ImportedAt) || len(in.Events) > MaxEvents {
		return ErrInvalid
	}
	if in.Source != SourceDemo && in.Source != SourceWUDA {
		return ErrInvalid
	}
	if in.Authoritative && in.Source != SourceWUDA {
		return ErrInvalid
	}
	for _, e := range in.Events {
		if !validText(e.ID, MaxEventID) || !validText(e.Title, MaxEventTitle) || (e.Type != "" && !validText(e.Type, MaxEventType)) || (e.Description != "" && !validText(e.Description, MaxEventDescription)) || e.StartAt.IsZero() || !e.EndAt.After(e.StartAt) || !e.StartAt.Before(in.ValidUntil) || e.EndAt.After(in.ValidUntil) {
			return ErrInvalid
		}
	}
	return nil
}

func validText(value string, max int) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= max
}

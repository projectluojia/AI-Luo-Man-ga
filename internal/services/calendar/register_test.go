package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/memory"
	cal "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/academiccalendar"
	"testing"
	"time"
)

func TestDispatcherCalendarAppIsolationAndGovernance(t *testing.T) {
	s := memory.NewCalendarStore()
	now := time.Now().UTC()
	if err := s.ReplaceSnapshot(context.Background(), cal.SnapshotInput{AppID: "campus-services", Revision: "r1", Source: "demo", Authoritative: false, Complete: true, ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour), Events: []cal.Event{{ID: "e", Title: "开学", StartAt: now, EndAt: now.Add(time.Hour)}}}); err != nil {
		t.Fatal(err)
	}
	r := registry.New()
	if err := Register(r, s); err != nil {
		t.Fatal(err)
	}
	p := runtimetest.NewStaticAppPolicy()
	p.Enable("campus-services", CapabilityID)
	d := runtime.NewDispatcher(r, p, runtime.DispatcherConfig{})
	b, _ := json.Marshal(cal.QueryRequest{From: now.Add(-time.Minute), To: now.Add(2 * time.Hour)})
	_, err := d.InvokeCapability(context.Background(), contracts.RequestContext{AppID: "campus-services", EchoID: "e", RequestID: "r", Deadline: now.Add(time.Minute), ProtocolVersion: "1.0"}, CapabilityID, b)
	if !errors.Is(err, cal.ErrDataUntrusted) {
		t.Fatalf("err=%v", err)
	}
}

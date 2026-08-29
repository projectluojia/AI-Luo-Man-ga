package access

import (
	"context"
	"fmt"
	"sync"
)

// AdmissionGate 管理接入事务的停止接纳与排空。
type AdmissionGate struct {
	mu        sync.Mutex
	wg        sync.WaitGroup
	accepting bool
}

// NewAdmissionGate 构造一个处于接纳状态的接入屏障。
func NewAdmissionGate() *AdmissionGate {
	return &AdmissionGate{accepting: true}
}

// Accepting 返回是否仍接受新的接入事务。
func (g *AdmissionGate) Accepting() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.accepting
}

// Begin 登记一条接入事务；停止接纳后返回 false。
func (g *AdmissionGate) Begin() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.accepting {
		return false
	}
	g.wg.Add(1)
	return true
}

// Done 标记一条接入事务完成。
func (g *AdmissionGate) Done() {
	g.wg.Done()
}

// StopAccepting 停止接纳新的接入事务。
func (g *AdmissionGate) StopAccepting() {
	g.mu.Lock()
	g.accepting = false
	g.mu.Unlock()
}

// WaitAdmissions 等待已经登记的接入事务完成。
func (g *AdmissionGate) WaitAdmissions(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("等待接入事务完成：%w", ctx.Err())
	}
}

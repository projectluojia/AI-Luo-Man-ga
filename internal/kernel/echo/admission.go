package echo

import "context"

// Admission 是所有外部入口创建 Echo 的统一用例端口。
type Admission interface {
	Create(context.Context, RunRequest) (string, bool, error)
}

// EchoAdmission 将持久 Echo 创建与队列通知绑定为一个入口操作。
// Echo 已持久化时才通知调度器；重复请求不会重复入队。
type EchoAdmission struct {
	creator  Creator
	enqueuer Enqueuer
}

// NewAdmission 构造统一 Echo Admission。
func NewAdmission(creator Creator, enqueuer Enqueuer) *EchoAdmission {
	if creator == nil || enqueuer == nil {
		panic("echo admission dependencies are incomplete")
	}
	return &EchoAdmission{creator: creator, enqueuer: enqueuer}
}

func (a *EchoAdmission) Create(ctx context.Context, request RunRequest) (string, bool, error) {
	echoID, created, err := a.creator.CreateIdempotent(ctx, request)
	if err != nil {
		return "", false, err
	}
	if created {
		a.enqueuer.Enqueue(ctx, echoID)
	}
	return echoID, created, nil
}

var _ Admission = (*EchoAdmission)(nil)

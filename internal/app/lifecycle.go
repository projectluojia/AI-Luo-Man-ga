package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/ingress"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/web"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/task"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/blob"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

// coreLifecycle 持有 Core 已创建的资源，并以固定顺序关闭它们。
// 顺序是：停止接入 → 排空请求 → 停止队列 → 释放执行者 → 停止运行时 →
// 停止后台任务 → 关闭 Blob 与数据库。
type coreLifecycle struct {
	server        *http.Server
	webAccess     *web.Server
	ingressServer *ingress.Server
	qqManager     *qq.Manager
	runScheduler  *kernelecho.Scheduler
	taskScheduler *task.Scheduler
	executorLease *loader.Lease
	runtimeLoader *loader.Manager
	blobStore     *blob.Store
	store         *sqlite.Store
}

const httpShutdownTimeout = 3 * time.Second

func (l *coreLifecycle) Shutdown(ctx context.Context) error {
	var shutdownErrors []error
	if l.webAccess != nil {
		l.webAccess.StopAccepting()
	}
	if l.ingressServer != nil {
		l.ingressServer.StopAccepting()
	}
	if l.webAccess != nil || l.ingressServer != nil {
		admissionDone := make(chan error, 2)
		if l.webAccess != nil {
			go func() { admissionDone <- l.webAccess.WaitAdmissions(ctx) }()
		} else {
			admissionDone <- nil
		}
		if l.ingressServer != nil {
			go func() { admissionDone <- l.ingressServer.WaitAdmissions(ctx) }()
		} else {
			admissionDone <- nil
		}
		shutdownErrors = append(shutdownErrors, <-admissionDone, <-admissionDone)
	}
	if l.server != nil {
		httpContext, cancel := context.WithTimeout(ctx, httpShutdownTimeout)
		httpShutdownErr := l.server.Shutdown(httpContext)
		cancel()
		if httpShutdownErr != nil {
			shutdownErrors = append(shutdownErrors, httpShutdownErr, l.server.Close())
		}
	}
	if l.qqManager != nil {
		shutdownErrors = append(shutdownErrors, l.qqManager.Shutdown(ctx))
	}
	if l.runScheduler != nil {
		shutdownErrors = append(shutdownErrors, l.runScheduler.Shutdown(ctx))
	}
	if l.executorLease != nil {
		l.executorLease.Release()
	}
	if l.runtimeLoader != nil {
		shutdownErrors = append(shutdownErrors, l.runtimeLoader.Shutdown(ctx))
	}
	if l.taskScheduler != nil {
		shutdownErrors = append(shutdownErrors, l.taskScheduler.Shutdown(ctx))
	}
	if l.blobStore != nil {
		shutdownErrors = append(shutdownErrors, l.blobStore.Close())
	}
	if l.store != nil {
		shutdownErrors = append(shutdownErrors, l.store.Close())
	}
	return errors.Join(shutdownErrors...)
}

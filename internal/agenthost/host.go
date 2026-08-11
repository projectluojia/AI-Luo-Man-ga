package agenthost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/agentv1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/agentprotocol"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Host struct {
	command *exec.Cmd
	done    chan struct{}
	errMu   sync.Mutex
	err     error
}

func Start(ctx context.Context, python, address string, stdout, stderr io.Writer) (*Host, error) {
	started := time.Now()
	if !isLoopbackAddress(address) {
		return nil, fmt.Errorf("Python AI Agent 非回环监听必须配置认证传输")
	}
	command := exec.Command(python, "-m", "agent.runtime", "--listen", address)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		observe.Error(ctx, "启动 Python AI Agent 进程失败", err,
			observe.Component("agent_host"),
			observe.StringAttr("address", address),
		)
		return nil, fmt.Errorf("启动 Python AI Agent：%w", err)
	}
	host := &Host{command: command, done: make(chan struct{})}
	go func() {
		err := command.Wait()
		host.errMu.Lock()
		host.err = err
		host.errMu.Unlock()
		close(host.done)
	}()
	observe.Info(ctx, "Python AI Agent 进程已经启动",
		observe.Component("agent_host"),
		observe.IntAttr("process_id", command.Process.Pid),
		observe.StringAttr("address", address),
		observe.Duration(started),
	)
	return host, nil
}

func (h *Host) Done() <-chan struct{} {
	return h.done
}

func (h *Host) Err() error {
	h.errMu.Lock()
	defer h.errMu.Unlock()
	return h.err
}

func (h *Host) Stop(ctx context.Context) error {
	select {
	case <-h.done:
		return h.Err()
	default:
	}
	if err := interruptProcess(h.command.Process); err != nil {
		select {
		case <-h.done:
			return h.Err()
		default:
			return fmt.Errorf("通知 Python AI Agent 停止：%w", err)
		}
	}
	select {
	case <-h.done:
		return normalizeExpectedStopError(h.Err())
	case <-ctx.Done():
		if err := h.command.Process.Kill(); err != nil {
			return errors.Join(ctx.Err(), fmt.Errorf("强制停止 Python AI Agent：%w", err))
		}
		select {
		case <-h.done:
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return errors.Join(ctx.Err(), fmt.Errorf("Python AI Agent 强制停止后未退出"))
		}
	}
}

func Dial(ctx context.Context, address string) (*grpc.ClientConn, agentv1.AgentRuntimeClient, error) {
	if !isLoopbackAddress(address) {
		return nil, nil, fmt.Errorf("Python AI Agent 非回环连接必须配置认证传输")
	}
	started := time.Now()
	observe.Debug(ctx, "正在连接 Python AI Agent",
		observe.Component("agent_host"),
		observe.StringAttr("address", address),
	)
	connection, err := grpc.DialContext(
		ctx,
		address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(agentprotocol.MaxGRPCMessageBytes),
			grpc.MaxCallSendMsgSize(agentprotocol.MaxGRPCMessageBytes),
		),
	)
	if err != nil {
		observe.Error(ctx, "连接 Python AI Agent 失败", err,
			observe.Component("agent_host"),
			observe.StringAttr("address", address),
			observe.Duration(started),
		)
		return nil, nil, fmt.Errorf("连接 Python AI Agent %s：%w", address, err)
	}
	observe.Info(ctx, "Python AI Agent 连接已经建立",
		observe.Component("agent_host"),
		observe.StringAttr("address", address),
		observe.Duration(started),
	)
	return connection, agentv1.NewAgentRuntimeClient(connection), nil
}

func isLoopbackAddress(address string) bool {
	if strings.HasPrefix(address, "unix:") {
		return true
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

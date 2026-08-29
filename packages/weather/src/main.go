// main 启动 weather isolated provider：监听清单 [component.process].address
// 指定的本机回环地址，实现 runtime_host v3 协议供内核装载与调用。
package main

import (
	"fmt"
	"net"
	"os"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/runtimev1"
	"github.com/projectluojia/weather/src/wx"

	"google.golang.org/grpc"
)

func main() {
	// 地址与身份由安装锁（ailuo.toml [component.process]）决定，经环境传入或用默认。
	listenAddress := os.Getenv("AILUO_LISTEN_ADDRESS")
	if listenAddress == "" {
		listenAddress = "127.0.0.1:50071"
	}
	service := wx.NewService(wx.NewClient(wx.ClientConfig{}))
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(512<<10),
		grpc.MaxSendMsgSize(512<<10),
	)
	runtimev1.RegisterRuntimeHostServer(server, newHostServer(service))
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		fmt.Fprintln(os.Stderr, "weather provider listen failed:", err)
		os.Exit(1)
	}
	if err := server.Serve(listener); err != nil {
		fmt.Fprintln(os.Stderr, "weather provider serve failed:", err)
		os.Exit(1)
	}
}

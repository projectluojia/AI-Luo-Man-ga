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
	listenAddress, err := listenAddressFromArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	service := wx.NewService(wx.NewClient(wx.ClientConfig{}))
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(512<<10),
		grpc.MaxSendMsgSize(512<<10),
	)
	runtimev1.RegisterRuntimeHostServer(server, newHostServer(service))
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		fmt.Fprintln(os.Stderr, "weather provider 监听失败:", err)
		os.Exit(1)
	}
	if err := server.Serve(listener); err != nil {
		fmt.Fprintln(os.Stderr, "weather provider 服务失败:", err)
		os.Exit(1)
	}
}

// listenAddressFromArgs 只接受安装锁展开后的 --listen <地址>，没有环境变量或默认端口。
func listenAddressFromArgs(args []string) (string, error) {
	for index := 0; index < len(args); index++ {
		if args[index] != "--listen" {
			continue
		}
		if index+1 >= len(args) || args[index+1] == "" {
			return "", fmt.Errorf("缺少 --listen 地址")
		}
		return args[index+1], nil
	}
	return "", fmt.Errorf("缺少 --listen 地址")
}

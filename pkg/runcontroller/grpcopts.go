package runcontroller

import (
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// GRPCServerOptions 返回控制面 gRPC 服务端的选项，取值照接口清单
// “gRPC keepalive”一节。
//
//   - 强制策略允许看守进程每 10 s 一次的 ping：MinTime 取 5 s，且没有活动流
//     时也允许，否则服务端会以 too_many_pings 断开看守进程。
//   - 服务端自己也探活：连接空闲 10 s 后发 ping，5 s 内没有应答就断开。actor
//     的 HTTP/2 在出口网关终止，看守进程的 ping 只到网关，网关到控制面这一段
//     只能由服务端自己发现已经断开的连接。
func GRPCServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    10 * time.Second,
			Timeout: 5 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	}
}

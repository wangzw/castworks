package runcontroller

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	agentprotocolv1 "github.com/wangzw/castworks/api/agentprotocol/v1"
)

// keepaliveTestStream 是测试用的 AgentStream 服务端。echo 为真时把收到的帧
// 回一个空 ControlFrame，用来断言流仍然可收发；为假时只在处理函数的 ctx 结束
// 后返回，用来断言服务端探活断开了死连接。
type keepaliveTestStream struct {
	agentprotocolv1.UnimplementedAgentStreamServer

	echo bool

	startedOnce sync.Once
	started     chan struct{}
	doneOnce    sync.Once
	done        chan struct{}

	mu     sync.Mutex
	ctxErr error
}

func newKeepaliveTestStream(echo bool) *keepaliveTestStream {
	return &keepaliveTestStream{
		echo:    echo,
		started: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (s *keepaliveTestStream) Connect(stream grpc.BidiStreamingServer[agentprotocolv1.WardenFrame, agentprotocolv1.ControlFrame]) error {
	s.startedOnce.Do(func() { close(s.started) })
	defer func() {
		s.mu.Lock()
		s.ctxErr = stream.Context().Err()
		s.mu.Unlock()
		s.doneOnce.Do(func() { close(s.done) })
	}()

	if !s.echo {
		<-stream.Context().Done()
		return stream.Context().Err()
	}

	for {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := stream.Send(&agentprotocolv1.ControlFrame{RunId: frame.GetRunId()}); err != nil {
			return err
		}
	}
}

func (s *keepaliveTestStream) contextErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctxErr
}

// startKeepaliveTestServer 在 127.0.0.1 的真实 TCP 回环上启动一个只注册了
// AgentStream 的服务端。bufconn 不经过 HTTP/2 的传输层 keepalive，这里不能用。
func startKeepaliveTestServer(t *testing.T, stub *keepaliveTestStream, opts ...grpc.ServerOption) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(opts...)
	agentprotocolv1.RegisterAgentStreamServer(srv, stub)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()

	t.Cleanup(func() {
		srv.Stop()
		if err := <-serveErr; err != nil {
			t.Errorf("grpc server: %v", err)
		}
	})
	return lis.Addr().String()
}

// startKeepaliveTestProxy 在 TCP 上把连接转发到 target，返回前端地址与一个停止
// 转发的函数。停止之后两侧的连接都不关闭，只是不再传递任何字节，模拟一条被
// 中间设备静默截断、但两端都看不出断开的通路。
func startKeepaliveTestProxy(t *testing.T, target string) (addr string, stopForwarding func()) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}

	stop := make(chan struct{})
	var stopOnce sync.Once
	stopForwarding = func() { stopOnce.Do(func() { close(stop) }) }
	// release 与 stop 分开：停止转发后两侧的连接必须保持打开，直到测试结束，
	// 否则服务端看到的是一条正常关闭的连接，探活没有被测到。
	release := make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)

		client, err := lis.Accept()
		if err != nil {
			return
		}
		defer func() { _ = client.Close() }()

		server, err := net.Dial("tcp", target)
		if err != nil {
			return
		}
		defer func() { _ = server.Close() }()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); keepaliveForward(server, client, stop) }()
		go func() { defer wg.Done(); keepaliveForward(client, server, stop) }()
		wg.Wait()
		<-release
	}()

	t.Cleanup(func() {
		stopForwarding()
		_ = lis.Close()
		close(release)
		<-done
	})
	return lis.Addr().String(), stopForwarding
}

// keepaliveForward 按短读超时轮询，收到 stop 后停止转发，但保留两端连接。
func keepaliveForward(dst, src net.Conn, stop <-chan struct{}) {
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-stop:
			return
		default:
		}

		_ = src.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, err := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			return
		}
	}
}

func dialKeepaliveTestClient(t *testing.T, addr string, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()

	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close client connection: %v", err)
		}
	})
	return conn
}

// TestGRPCServerOptionsEnforcementPolicy 验证强制策略允许看守进程每 10 s 一次的
// ping：把服务端自己的 ping 关掉（后追加的选项覆盖前一个），只留下强制策略，
// 一条空闲的流保持 35 s 后仍能收发。
//
// 强制策略缺失或 MinTime 大于 10 s 时，客户端会先收到 too_many_pings 的 GOAWAY，
// 随后的收发以 Unavailable 失败。
func TestGRPCServerOptionsEnforcementPolicy(t *testing.T) {
	stub := newKeepaliveTestStream(true)
	opts := append(GRPCServerOptions(), grpc.KeepaliveParams(keepalive.ServerParameters{
		Time: 2 * time.Hour,
	}))
	addr := startKeepaliveTestServer(t, stub, opts...)

	conn := dialKeepaliveTestClient(t, addr, grpc.WithKeepaliveParams(keepalive.ClientParameters{
		Time:                10 * time.Second,
		Timeout:             5 * time.Second,
		PermitWithoutStream: true,
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	stream, err := agentprotocolv1.NewAgentStreamClient(conn).Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	roundTrip := func(when string) {
		t.Helper()
		if err := stream.Send(&agentprotocolv1.WardenFrame{RunId: "run-grpcopts"}); err != nil {
			// 连接被 too_many_pings 断开时 Send 常只返回 EOF，随后的 Recv
			// 带出真正的状态码。
			_, recvErr := stream.Recv()
			t.Fatalf("%s: send: %v; recv: %v", when, err, recvErr)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("%s: recv: %v", when, err)
		}
	}

	roundTrip("after connecting")

	// 这段时间里唯一的流量是客户端每 10 s 一次的 keepalive ping。
	time.Sleep(35 * time.Second)

	roundTrip("after 35s idle")
}

// TestGRPCServerOptionsServerPing 验证服务端自己的探活：客户端不开 keepalive，
// 流建立后经测试内的 TCP 转发连接停止转发，两端都不关闭连接；服务端空闲 10 s
// 后 ping，5 s 内收不到应答就断开，处理函数的 ctx 在 20 s 内结束。
//
// 去掉 ServerParameters 时连接不会被断开，处理函数的 ctx 一直不结束，这里超时失败。
func TestGRPCServerOptionsServerPing(t *testing.T) {
	stub := newKeepaliveTestStream(false)
	serverAddr := startKeepaliveTestServer(t, stub, GRPCServerOptions()...)
	proxyAddr, stopForwarding := startKeepaliveTestProxy(t, serverAddr)

	// 不传 WithKeepaliveParams：客户端 keepalive 默认关闭。
	conn := dialKeepaliveTestClient(t, proxyAddr)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	stream, err := agentprotocolv1.NewAgentStreamClient(conn).Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := stream.Send(&agentprotocolv1.WardenFrame{RunId: "run-grpcopts"}); err != nil {
		t.Fatalf("send first frame: %v", err)
	}

	select {
	case <-stub.started:
	case <-time.After(10 * time.Second):
		t.Fatal("server handler did not start")
	}

	stopForwarding()

	select {
	case <-stub.done:
	case <-time.After(20 * time.Second):
		t.Fatal("server handler context did not end within 20s; the server did not probe the dead connection")
	}

	if err := stub.contextErr(); !errors.Is(err, context.Canceled) {
		t.Fatalf("server handler context error = %v, want context.Canceled", err)
	}
}

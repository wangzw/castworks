// Package readyz 提供 Substrate wakeupProbe 轮询的就绪探针。
//
// 对外接口：
//
//	func Start(addr string) (*Server, error)
//	func (s *Server) Addr() net.Addr
//	func (s *Server) Close() error
//
// Start 监听 addr（例如 ":8081"）并在后台提供 GET /readyz；监听成功即返回，
// 之后任意时刻的请求都返回 200，不以控制面连接或任何其他状态为条件。
// Close 关闭监听与在途连接，可重复调用。
package readyz

import (
	"fmt"
	"net"
	"net/http"
)

// Server 是运行中的就绪探针服务。
type Server struct {
	httpServer *http.Server
	listener   net.Listener
	done       chan struct{}
}

// Start 在 addr 上监听并在后台提供 GET /readyz。
func Start(addr string) (*Server, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("readyz: listen %s: %w", addr, err)
	}

	s := &Server{
		listener: listener,
		done:     make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", handleReadyz)
	s.httpServer = &http.Server{Handler: mux}

	go func() {
		defer close(s.done)
		// Serve 在 Close 后返回 ErrServerClosed；其余错误说明监听不可用，
		// 探针本身就绪情况由请求结果反映，这里不再重复上报。
		_ = s.httpServer.Serve(listener)
	}()

	return s, nil
}

// Addr 返回实际监听的地址；addr 为 ":0" 或 "127.0.0.1:0" 时据此取得分配的端口。
func (s *Server) Addr() net.Addr {
	return s.listener.Addr()
}

// Close 关闭监听与在途连接，等待服务循环退出。
func (s *Server) Close() error {
	err := s.httpServer.Close()
	<-s.done
	return err
}

func handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte("ok\n"))
	}
}

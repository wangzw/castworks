package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wangzw/castworks/pkg/humanchannel"
	"github.com/wangzw/castworks/pkg/runcontroller"
	"github.com/wangzw/castworks/pkg/sandboxprovider"
)

const (
	// databaseConnectTimeout bounds the startup Ping. It does not bound the
	// queries the controller issues later.
	databaseConnectTimeout = 10 * time.Second

	// httpReadHeaderTimeout bounds reading a request head. There is no overall
	// read or write timeout: a request may wait on the controller.
	httpReadHeaderTimeout = 10 * time.Second
)

// 关闭顺序的记录点。HTTP 服务端先停，使新请求不再进入；controller.Run 随后
// 返回，等在途的提供者调用结束；gRPC 服务端最后停，因为看守进程的流是长连接，
// 必须等 controller 不再需要它；然后释放提供者与连接池。
const (
	shutdownStepHTTP       = "http"
	shutdownStepController = "controller"
	shutdownStepGRPC       = "grpc"
	shutdownStepProvider   = "provider"
	shutdownStepPool       = "pool"
)

// usageError 标记由命令行参数引起的错误，main 据此以退出码 2 结束。
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }

func (e *usageError) Unwrap() error { return e.err }

// runHooks 是 run 的测试挂钩，所有字段都可以为 nil。
type runHooks struct {
	// ready 在两个监听器绑定、服务端与 controller.Run 都已启动后调用，
	// 参数是 HTTP 与 gRPC 实际监听的地址。
	ready func(httpAddr, grpcAddr string)
	// shutdownStep 在关闭流程的每一步完成后调用，参数是上面的 shutdownStep
	// 常量之一。
	shutdownStep func(step string)
	// controllerContext 在启动 controller.Run 之前用它的 ctx 调用。测试用它
	// 检查信号到来后这个 ctx 在 HTTP 关闭期间仍然有效。
	controllerContext func(ctx context.Context)
	// listeners 在服务端启动、ready 返回后调用，参数是两个已绑定的监听器。
	// 测试用它关闭监听器，模拟 Serve 意外退出。
	listeners func(httpLn, grpcLn net.Listener)
	// httpServer 在 http.Server 构造完成、开始服务之前调用。测试用它安装
	// ConnState，观察已建立的连接。
	httpServer func(server *http.Server)
	// newProvider 若非 nil，取代 newProvider。测试用它模拟构造提供者期间
	// 收到信号。
	newProvider func(ctx context.Context, cfg *Config) (sandboxprovider.Provider, error)
	// runController 若非 nil，取代 controller.Run。测试用它让 Run 晚于关闭
	// 期限返回，检查期限分支。
	runController func(ctx context.Context) error
}

// run 以同进程方式启动控制面并阻塞到 ctx 结束或某个服务意外退出。它不在
// 这里设置 slog 默认日志器，那是 main 与测试的 TestMain 的职责。
func run(ctx context.Context, args []string, ready func(httpAddr, grpcAddr string)) error {
	return runWithHooks(ctx, args, runHooks{ready: ready})
}

// runWithHooks 是 run 的实现。启动按下面的顺序进行：
//
//	解析并校验参数 -> 读凭据 -> 连库并 Ping -> Migrate -> 构造提供者 ->
//	runcontroller.New -> humanchannel.NewHandler -> 监听 HTTP 与 gRPC ->
//	注册 gRPC 服务 -> 启动 controller.Run -> 开始服务。
//
// 任何一步失败都释放已建立的资源后返回错误，且失败发生在监听之前时不会有
// 任何端口被打开。
func runWithHooks(ctx context.Context, args []string, hooks runHooks) error {
	cfg, err := ParseConfig(args, os.Getenv)
	if err != nil {
		return &usageError{err: err}
	}

	runCfg, suspendForcedOff := cfg.RunControllerConfig()
	credential, err := readLLMCredential(cfg.LLMCredentialFile, cfg.LLMCredentialKind)
	if err != nil {
		return err
	}
	runCfg.Credential = credential

	// The summary never carries the credential value or the database URL.
	slog.Default().Info("config",
		"provider", cfg.Provider,
		"suspend", runCfg.SuspendEnabled,
		"credential_kind", credential.Kind)
	if suspendForcedOff {
		slog.Default().Info("suspend forced off")
	}

	pool, err := connectDatabase(ctx, cfg.DatabaseURL)
	if err != nil {
		return startupResult(ctx, err)
	}
	if err := runcontroller.Migrate(ctx, pool); err != nil {
		pool.Close()
		return startupResult(ctx, fmt.Errorf("migrate database: %w", err))
	}

	constructProvider := newProvider
	if hooks.newProvider != nil {
		constructProvider = hooks.newProvider
	}
	provider, err := constructProvider(ctx, cfg)
	if err != nil {
		pool.Close()
		return startupResult(ctx, err)
	}

	controller := runcontroller.New(runCfg, pool, provider, runcontroller.RealClock())
	handler := humanchannel.NewHandler(controller)

	httpLn, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		releaseAfterStartupFailure(provider)
		pool.Close()
		return fmt.Errorf("--http-addr: listen on %q: %w", cfg.HTTPAddr, err)
	}
	// Re-check the bound address: the flag validation only looked at the
	// requested address, and the listener is the authoritative one.
	if err := checkLoopbackListener(httpLn); err != nil {
		_ = httpLn.Close()
		releaseAfterStartupFailure(provider)
		pool.Close()
		return fmt.Errorf("--http-addr: %w", err)
	}

	grpcLn, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		_ = httpLn.Close()
		releaseAfterStartupFailure(provider)
		pool.Close()
		return fmt.Errorf("--grpc-addr: listen on %q: %w", cfg.GRPCAddr, err)
	}

	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}
	if hooks.httpServer != nil {
		hooks.httpServer(httpServer)
	}
	grpcServer := grpc.NewServer(runcontroller.GRPCServerOptions()...)
	controller.RegisterGRPC(grpcServer)

	return serve(ctx, cfg, hooks, pool, provider, controller, httpServer, grpcServer, httpLn, grpcLn)
}

// startupResult reports a startup failure that is only the signal context
// ending as a normal shutdown. The plan treats a signal received during
// Migrate or provider construction as a normal signal exit, which is exit 0;
// any other startup failure is returned unchanged. gRPC reports a canceled
// call as a status error instead of wrapping context.Canceled, so both forms
// count as the signal.
func startupResult(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil {
		return err
	}
	if errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
		return nil
	}
	return err
}

// connectDatabase parses the URL, opens the pool and waits for the first
// connection. The pool is owned by the caller.
func connectDatabase(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse --database-url: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, databaseConnectTimeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(pingCtx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}

// checkLoopbackListener re-checks the actually bound HTTP address.
func checkLoopbackListener(ln net.Listener) error {
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("listener address %s is not a TCP address", ln.Addr())
	}
	if !addr.IP.IsLoopback() {
		return fmt.Errorf("bound address %s is not a loopback address", addr)
	}
	return nil
}

// releaseAfterStartupFailure closes a provider while the process is still
// starting. A close error is reported but does not replace the startup error.
func releaseAfterStartupFailure(provider sandboxprovider.Provider) {
	if err := closeProvider(provider); err != nil {
		slog.Default().Warn("close sandbox provider after startup failure", "error", err)
	}
}

// serve 启动两个服务端与控制器，等到 ctx 结束或某个服务意外退出，然后按
// shutdownStep 常量的顺序关闭全部资源。
func serve(
	ctx context.Context,
	cfg *Config,
	hooks runHooks,
	pool *pgxpool.Pool,
	provider sandboxprovider.Provider,
	controller *runcontroller.Controller,
	httpServer *http.Server,
	grpcServer *grpc.Server,
	httpLn, grpcLn net.Listener,
) error {
	httpAddr := httpLn.Addr().String()
	grpcAddr := grpcLn.Addr().String()

	// controller.Run runs even when suspension is disabled: it still performs
	// the T_hello checks and is the owner of in-flight provider calls.
	//
	// Its context deliberately does not inherit ctx's cancellation. A signal
	// must stop HTTP first and leave in-flight provider calls running until
	// step 2 cancels them; deriving runCtx from ctx would make it exit in
	// lockstep with the signal. Values are still inherited.
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()
	if hooks.controllerContext != nil {
		hooks.controllerContext(runCtx)
	}
	runController := controller.Run
	if hooks.runController != nil {
		runController = hooks.runController
	}
	runErr := make(chan error, 1)
	go func() { runErr <- runController(runCtx) }()

	httpErr := make(chan error, 1)
	go func() {
		err := httpServer.Serve(httpLn)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		httpErr <- err
	}()

	grpcErr := make(chan error, 1)
	go func() { grpcErr <- grpcServer.Serve(grpcLn) }()

	slog.Default().Info("http listening", "addr", httpAddr)
	slog.Default().Info("grpc listening", "addr", grpcAddr)
	if hooks.ready != nil {
		hooks.ready(httpAddr, grpcAddr)
	}
	if hooks.listeners != nil {
		hooks.listeners(httpLn, grpcLn)
	}

	var cause error
	httpExited, grpcExited := false, false
	var httpServeErr, grpcServeErr error
	select {
	case <-ctx.Done():
	case err := <-httpErr:
		httpExited = true
		httpServeErr = err
		if err != nil {
			cause = fmt.Errorf("http server: %w", err)
		} else {
			cause = errors.New("http server stopped unexpectedly")
		}
	case err := <-grpcErr:
		grpcExited = true
		grpcServeErr = err
		if err != nil {
			cause = fmt.Errorf("grpc server: %w", err)
		} else {
			cause = errors.New("grpc server stopped unexpectedly")
		}
	}
	if cause != nil {
		slog.Default().Error("server exited unexpectedly", "error", cause)
	}

	deadline := time.Now().Add(cfg.ShutdownTimeout)
	remaining := func() time.Duration {
		if d := time.Until(deadline); d > 0 {
			return d
		}
		return 0
	}

	// 1. HTTP first: stop accepting and end the connections that are already
	// established, letting in-flight requests finish until the deadline. This
	// runs even when Serve has already returned: Serve closes only the
	// listener, and an accepted connection keeps calling the controller until
	// the server closes it. Close is the deadline fallback, because Shutdown
	// only closes idle connections.
	if d := remaining(); d <= 0 {
		logDeadlineExhausted(shutdownStepHTTP)
		_ = httpServer.Close()
	} else {
		shutdownCtx, cancelShutdown := context.WithDeadline(context.Background(), deadline)
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.Default().Warn("http shutdown exceeded the deadline", "error", err)
			_ = httpServer.Close()
		}
		cancelShutdown()
	}
	if !httpExited {
		httpServeErr = <-httpErr
	}
	if httpServeErr != nil {
		slog.Default().Warn("http server stopped with an error", "error", httpServeErr)
	}
	recordShutdownStep(hooks, shutdownStepHTTP)

	// 2. Cancel controller.Run and wait for its in-flight provider calls. When
	// an earlier step used up the deadline, do not present the zero-length
	// wait as a controller timeout: only the already-finished result is
	// taken, and the reason stays the exhausted deadline.
	cancelRun()
	controllerStopped := false
	var controllerStopErr error
	if d := remaining(); d <= 0 {
		logDeadlineExhausted(shutdownStepController)
		select {
		case err := <-runErr:
			if err != nil {
				slog.Default().Warn("controller.Run returned an error", "error", err)
			}
			slog.Default().Info("controller stopped")
			controllerStopped = true
		default:
			controllerStopErr = errors.New("shutdown deadline exhausted before controller.Run stopped")
		}
	} else {
		select {
		case err := <-runErr:
			if err != nil {
				slog.Default().Warn("controller.Run returned an error", "error", err)
			}
			slog.Default().Info("controller stopped")
			controllerStopped = true
		case <-time.After(d):
			slog.Default().Error("controller did not stop before the shutdown deadline")
			controllerStopErr = errors.New("controller.Run did not stop before the shutdown deadline")
		}
	}
	recordShutdownStep(hooks, shutdownStepController)

	// 3. gRPC last. GracefulStop waits for streams, so the deadline turns it
	// into Stop: the warden's streams are long-lived and never end by
	// themselves. Like the HTTP server, this runs even when Serve has already
	// returned, because closing the listener does not end established streams.
	// This step runs even when Run did not stop; only the provider and the
	// pool are skipped below.
	if d := remaining(); d <= 0 {
		logDeadlineExhausted(shutdownStepGRPC)
		grpcServer.Stop()
	} else {
		gracefulDone := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(gracefulDone)
		}()
		select {
		case <-gracefulDone:
		case <-time.After(d):
			slog.Default().Warn("grpc graceful stop exceeded the deadline; stopping")
			grpcServer.Stop()
			<-gracefulDone
		}
	}
	if !grpcExited {
		grpcServeErr = <-grpcErr
	}
	if grpcServeErr != nil {
		slog.Default().Warn("grpc server stopped with an error", "error", grpcServeErr)
	}
	recordShutdownStep(hooks, shutdownStepGRPC)
	slog.Default().Info("servers stopped")

	if !controllerStopped {
		// Run may still be using the provider and the pool. Closing either
		// underneath it would race with the in-flight provider calls, so leave
		// both to process exit and report a failed shutdown.
		slog.Default().Error("shutdown aborted: controller.Run is still running; provider and pool left open")
		return controllerStopErr
	}

	// 4. Release the provider, then the pool it no longer needs. Both are
	// bounded by what remains of the overall shutdown deadline. When nothing
	// remains, skip them rather than racing a zero-length timer.
	if d := remaining(); d <= 0 {
		logDeadlineExhausted(shutdownStepProvider)
	} else if err := closeProviderWithin(provider, d); err != nil {
		slog.Default().Error("close sandbox provider", "error", err)
	}
	recordShutdownStep(hooks, shutdownStepProvider)

	if d := remaining(); d <= 0 {
		logDeadlineExhausted(shutdownStepPool)
	} else if !closePoolWithin(pool, d) {
		slog.Default().Error("database pool close exceeded the shutdown deadline")
	}
	recordShutdownStep(hooks, shutdownStepPool)

	slog.Default().Info("shutdown complete")
	return cause
}

// logDeadlineExhausted reports that an earlier shutdown step used up the
// deadline. The step is still recorded, but it no longer has a budget to wait
// or to close with.
func logDeadlineExhausted(step string) {
	slog.Default().Error("shutdown deadline exhausted; skipping step", "step", step)
}

// closeProviderWithin releases the provider but does not wait longer than
// budget for it. A timed-out Close keeps running in its goroutine; the process
// is exiting, so its resources go with it.
func closeProviderWithin(provider sandboxprovider.Provider, budget time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- closeProvider(provider) }()
	select {
	case err := <-done:
		return err
	case <-time.After(budget):
		return fmt.Errorf("timed out after %s before the shutdown deadline", budget)
	}
}

// closePoolWithin closes the pool but does not wait longer than budget for it.
// It reports false on a timeout.
func closePoolWithin(pool *pgxpool.Pool, budget time.Duration) bool {
	done := make(chan struct{})
	go func() {
		pool.Close()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(budget):
		return false
	}
}

func recordShutdownStep(hooks runHooks, step string) {
	if hooks.shutdownStep != nil {
		hooks.shutdownStep(step)
	}
}

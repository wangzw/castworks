// Command netprobe-server runs the development-host side of the netprobe:
// a cleartext HTTP/2 (h2c) gRPC server that echoes every message on a
// bidirectional stream. The sandbox-side client reaches it through the
// Substrate egress gateway by dialing the in-cluster Service name
// castworks-dev.castworks-system.svc.cluster.local:9090.
//
// The service is described by a hand-written grpc.ServiceDesc, so the probe
// needs no generated protocol code. Messages are google.protobuf.StringValue
// values carried by the default proto codec.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	serviceName = "netprobe.Echo"
	// bidiMethod is the full method path the client invokes.
	bidiMethod = "/" + serviceName + "/Bidi"
	// reportMethod returns when the last stream closed, so the client can
	// compare the server's detection time with its own after a cut.
	reportMethod = "/" + serviceName + "/Report"
)

// streamClose is the server-side record of the last stream that ended.
type streamClose struct {
	At              string  `json:"at"`
	Received        int     `json:"received"`
	Sent            int     `json:"sent"`
	DurationSeconds float64 `json:"durationSeconds"`
	Error           string  `json:"error,omitempty"`
}

type serverReport struct {
	StreamsClosed int         `json:"streamsClosed"`
	LastClose     streamClose `json:"lastClose"`
}

var (
	reportMu      sync.Mutex
	streamsClosed int
	lastClose     streamClose
)

// echoServerAPI is the empty interface the registered implementation must
// satisfy; grpc.ServiceDesc requires a pointer to an interface type.
type echoServerAPI interface{}

type echoServer struct{}

// bidi reads a message, echoes it back, and repeats until the client
// half-closes the stream. Every message and the stream lifetime are logged so
// the server-side raw output can be checked against the client's counts. The
// close is also recorded for the Report RPC, which tells the client whether
// the server noticed a cut before or after it did.
func (echoServer) bidi(_ any, stream grpc.ServerStream) error {
	start := time.Now()
	log.Printf("bidi stream opened at %s", start.UTC().Format(time.RFC3339Nano))
	received, sent := 0, 0
	var closeErr error
	defer func() {
		recordStreamClose(start, received, sent, closeErr)
	}()
	for {
		msg := new(wrapperspb.StringValue)
		if err := stream.RecvMsg(msg); err != nil {
			closeErr = err
			log.Printf("bidi stream closed at %s: recv_err=%v received=%d sent=%d duration=%s",
				time.Now().UTC().Format(time.RFC3339Nano), err, received, sent,
				time.Since(start).Round(time.Millisecond))
			// A client half-close (io.EOF) is the normal end of the probe.
			return nil
		}
		received++
		log.Printf("recv n=%d value=%q at=%s", received, msg.GetValue(),
			time.Now().UTC().Format(time.RFC3339Nano))
		if err := stream.SendMsg(&wrapperspb.StringValue{Value: "echo:" + msg.GetValue()}); err != nil {
			closeErr = err
			log.Printf("send failed after received=%d sent=%d: %v", received, sent, err)
			return err
		}
		sent++
	}
}

func recordStreamClose(start time.Time, received, sent int, err error) {
	c := streamClose{
		At:              time.Now().UTC().Format(time.RFC3339Nano),
		Received:        received,
		Sent:            sent,
		DurationSeconds: time.Since(start).Seconds(),
	}
	// io.EOF is the client's normal half-close, not an error worth reporting.
	if err != nil && !errors.Is(err, io.EOF) {
		c.Error = err.Error()
	}
	reportMu.Lock()
	streamsClosed++
	lastClose = c
	reportMu.Unlock()
}

// reportHandler implements the hand-written unary Report method.
func reportHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(wrapperspb.StringValue)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return buildReport()
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: reportMethod}
	handler := func(context.Context, any) (any, error) { return buildReport() }
	return interceptor(ctx, in, info, handler)
}

func buildReport() (*wrapperspb.StringValue, error) {
	reportMu.Lock()
	rep := serverReport{StreamsClosed: streamsClosed, LastClose: lastClose}
	reportMu.Unlock()
	payload, err := json.Marshal(rep)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal server report: %v", err)
	}
	return wrapperspb.String(string(payload)), nil
}

// connLogger writes one line when a downstream connection begins and ends.
// The egress gateway may close the connection between the client's keepalive
// pings; these lines show whether the server ever saw that happen.
type connLogger struct{}

func (connLogger) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }
func (connLogger) HandleRPC(context.Context, stats.RPCStats)                       {}

func (connLogger) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	log.Printf("conn begin remote=%s local=%s at=%s", info.RemoteAddr, info.LocalAddr,
		time.Now().UTC().Format(time.RFC3339Nano))
	return ctx
}

func (connLogger) HandleConn(_ context.Context, s stats.ConnStats) {
	switch s.(type) {
	case *stats.ConnEnd:
		log.Printf("conn end at=%s", time.Now().UTC().Format(time.RFC3339Nano))
	}
}

func main() {
	addr := flag.String("addr", "10.12.72.11:9090", "listen address")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}

	// The sandbox client sends an HTTP/2 keepalive PING every 10s. Permit it
	// explicitly so the server does not answer with ENHANCE_YOUR_CALM.
	// Server-side keepalive is on as well: ping after 10s without activity and
	// give up the connection after 5s without an ack, which lets the logs show
	// whether the server saw a dead peer before the client did.
	srv := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    10 * time.Second,
			Timeout: 5 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.StatsHandler(connLogger{}),
	)

	desc := &grpc.ServiceDesc{
		ServiceName: serviceName,
		HandlerType: (*echoServerAPI)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Report",
			Handler:    reportHandler,
		}},
		Streams: []grpc.StreamDesc{{
			StreamName:    "Bidi",
			Handler:       echoServer{}.bidi,
			ServerStreams: true,
			ClientStreams: true,
		}},
		Metadata: "netprobe.proto",
	}
	srv.RegisterService(desc, echoServer{})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Printf("shutting down")
		srv.Stop()
	}()

	fmt.Printf("SERVER_READY addr=%s method=%s at=%s\n", *addr, bidiMethod,
		time.Now().UTC().Format(time.RFC3339Nano))
	os.Stdout.Sync()
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

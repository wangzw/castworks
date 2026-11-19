// Command netprobe runs inside the temporary Substrate actor and answers four
// questions about the egress path from an actor in atespace castworks-dev to
// the development host:
//
//	Q1  a cleartext h2c gRPC bidirectional stream to
//	    castworks-dev.castworks-system.svc.cluster.local:9090;
//	Q2  whether that stream survives five minutes with client keepalive
//	    pings every 10s (5s timeout) and messages in both directions;
//	Q3  a TLS handshake (plus one credential-free HTTP request) to
//	    api.anthropic.com:443 through a tls_passthrough rule;
//	Q4  whether a host outside the EgressPolicy, example.com, is refused;
//	Q5  whether a bidirectional stream survives a long application-idle
//	    period with only keepalive pings, and when and how it is cut if it
//	    is not. A cut is a valid conclusion, not a probe failure.
//
// The process also serves /readyz on port 8081, which the actor template uses
// as its wakeup probe. It prints one NETPROBE_Q<n> PASS|FAIL line per question
// with a JSON payload, then NETPROBE_DONE, and stays alive so the operator
// can read its logs.
//
// For a local smoke test, NETPROBE_TARGET may point at 127.0.0.1, Q2 and Q5
// durations may be shortened with NETPROBE_Q2_DURATION, NETPROBE_Q2_INTERVAL,
// NETPROBE_IDLE_QUIET and NETPROBE_IDLE_TOTAL, and NETPROBE_SKIP_EXTERNAL=1
// skips Q3 and Q4, which need the cluster egress policy.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	defaultTarget = "castworks-dev.castworks-system.svc.cluster.local:9090"
	bidiMethod    = "/netprobe.Echo/Bidi"
	reportMethod  = "/netprobe.Echo/Report"
)

// probeTarget may be overridden for local smoke tests; the actor run leaves it
// at the in-cluster Service name the EgressPolicy allows.
var probeTarget = envOr("NETPROBE_TARGET", defaultTarget)

var bidiDesc = &grpc.StreamDesc{
	StreamName:    "Bidi",
	ServerStreams: true,
	ClientStreams: true,
}

type q1Result struct {
	Pass             bool     `json:"pass"`
	Target           string   `json:"target"`
	Attempts         int      `json:"attempts"`
	AttemptErrors    []string `json:"attemptErrors,omitempty"`
	Echo             string   `json:"echo"`
	ConnectedAt      string   `json:"connectedAt"`
	ConnectElapsedMs int64    `json:"connectElapsedMs"`
}

type q2Result struct {
	Pass                    bool    `json:"pass"`
	Skipped                 bool    `json:"skipped,omitempty"`
	KeepalivePingSeconds    int     `json:"keepalivePingSeconds"`
	KeepaliveTimeoutSeconds int     `json:"keepaliveTimeoutSeconds"`
	StartTime               string  `json:"startTime"`
	EndTime                 string  `json:"endTime"`
	DurationSeconds         float64 `json:"durationSeconds"`
	MessagesSent            int     `json:"messagesSent"`
	MessagesReceived        int     `json:"messagesReceived"`
	FirstSeq                int     `json:"firstSeq"`
	LastSeq                 int     `json:"lastSeq"`
	Error                   string  `json:"error,omitempty"`
}

type q3Result struct {
	Pass            bool     `json:"pass"`
	Host            string   `json:"host"`
	Attempts        int      `json:"attempts"`
	TransientErrors []string `json:"transientErrors,omitempty"`
	TCPConnectMs    int64    `json:"tcpConnectMs"`
	HandshakeMs     int64    `json:"handshakeMs"`
	TLSVersion      string   `json:"tlsVersion"`
	CipherSuite     string   `json:"cipherSuite"`
	PeerSubject     string   `json:"peerSubject"`
	PeerIssuer      string   `json:"peerIssuer"`
	PeerNotAfter    string   `json:"peerNotAfter"`
	HTTPStatus      string   `json:"httpStatus"`
	CredentialUsed  bool     `json:"credentialUsed"`
	Error           string   `json:"error,omitempty"`
}

type q4Result struct {
	Pass       bool     `json:"pass"`
	Host       string   `json:"host"`
	Addrs      []string `json:"addrs,omitempty"`
	LookupErr  string   `json:"lookupError,omitempty"`
	DialErr    string   `json:"dialError,omitempty"`
	DialOK     bool     `json:"dialSucceeded"`
	ElapsedMs  int64    `json:"elapsedMs"`
	PolicyNote string   `json:"policyNote"`
}

// q5Result records the outcome of the application-idle phase. A cut is a
// valid conclusion: Pass is true both when the stream survived the whole test
// and when it was cut, as long as the cut was observed with a time and a gRPC
// status. Pass is false only when the test could not reach a conclusion.
type q5Result struct {
	Pass                    bool    `json:"pass"`
	Skipped                 bool    `json:"skipped,omitempty"`
	Target                  string  `json:"target"`
	KeepalivePingSeconds    int     `json:"keepalivePingSeconds"`
	KeepaliveTimeoutSeconds int     `json:"keepaliveTimeoutSeconds"`
	QuietSeconds            float64 `json:"quietSeconds"`
	ActiveSeconds           float64 `json:"activeSeconds"`
	TotalSeconds            float64 `json:"totalSeconds"`
	DurationSeconds         float64 `json:"durationSeconds"`
	Survived                bool    `json:"survived"`
	Cut                     bool    `json:"cut"`
	CutAfterSeconds         float64 `json:"cutAfterSeconds,omitempty"`
	CutPhase                string  `json:"cutPhase,omitempty"`
	GRPCStatusCode          string  `json:"grpcStatusCode,omitempty"`
	GRPCCode                uint32  `json:"grpcCode,omitempty"`
	GRPCMessage             string  `json:"grpcMessage,omitempty"`
	MessagesAfterIdle       int     `json:"messagesAfterIdle"`
	DetectedBy              string  `json:"detectedBy,omitempty"`
	ClientDetectedAt        string  `json:"clientDetectedAt,omitempty"`
	ServerDetectedAt        string  `json:"serverDetectedAt,omitempty"`
	DetectionDeltaMs        int64   `json:"detectionDeltaMs,omitempty"`
	ServerReportError       string  `json:"serverReportError,omitempty"`
	Error                   string  `json:"error,omitempty"`
}

// serverStreamReport is the payload of the server's Report RPC. The client
// uses the server's close time to decide which side noticed a cut first.
type serverStreamReport struct {
	StreamsClosed int             `json:"streamsClosed"`
	LastClose     streamCloseInfo `json:"lastClose"`
}

type streamCloseInfo struct {
	At              string  `json:"at"`
	Received        int     `json:"received"`
	Sent            int     `json:"sent"`
	DurationSeconds float64 `json:"durationSeconds"`
	Error           string  `json:"error,omitempty"`
}

func main() {
	// Serve the wakeup probe before anything else: ResumeActor returns only
	// after it answers 200, and the probes need the actor to stay RUNNING.
	go serveReady()

	fmt.Printf("NETPROBE_START target=%s at=%s\n", probeTarget, now())
	os.Stdout.Sync()

	delay := envDuration("NETPROBE_START_DELAY", 15*time.Second)
	time.Sleep(delay)

	if os.Getenv("NETPROBE_SKIP_EXTERNAL") == "1" {
		// Local smoke test: Q3 and Q4 depend on the cluster's egress policy.
		reportStatus("Q3", "SKIP", map[string]string{"reason": "NETPROBE_SKIP_EXTERNAL=1"})
		reportStatus("Q4", "SKIP", map[string]string{"reason": "NETPROBE_SKIP_EXTERNAL=1"})
	} else {
		q3 := runQ3()
		report("Q3", q3.Pass, q3)
		q4 := runQ4()
		report("Q4", q4.Pass, q4)
	}
	q1, q2 := runQ1Q2()
	report("Q1", q1.Pass, q1)
	report("Q2", q2.Pass, q2)
	if q1.Pass {
		q5 := runQ5()
		report("Q5", q5.Pass, q5)
	} else {
		report("Q5", false, q5Result{
			Target:  probeTarget,
			Skipped: true,
			Error:   "Q1 did not establish a stream",
		})
	}

	fmt.Printf("NETPROBE_DONE at=%s\n", now())
	os.Stdout.Sync()

	// Keep the container alive so the actor stays RUNNING and its logs remain
	// readable while the operator collects the result.
	select {}
}

func serveReady() {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	if err := http.ListenAndServe("0.0.0.0:8081", mux); err != nil {
		log.Fatalf("readyz listener: %v", err)
	}
}

func report(name string, pass bool, v any) {
	status := "FAIL"
	if pass {
		status = "PASS"
	}
	reportStatus(name, status, v)
}

func reportStatus(name, status string, v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		payload = []byte(`{"marshalError":"` + err.Error() + `"}`)
	}
	fmt.Printf("NETPROBE_%s %s %s\n", name, status, payload)
	os.Stdout.Sync()
}

// runQ1Q2 opens one bidirectional stream and answers Q1 with its first
// round-trip. On success the same stream carries Q2's message exchange under
// client keepalive for NETPROBE_Q2_DURATION (five minutes by default).
func runQ1Q2() (q1Result, q2Result) {
	q1 := q1Result{Target: probeTarget}
	q2 := q2Result{
		KeepalivePingSeconds:    10,
		KeepaliveTimeoutSeconds: 5,
	}
	q2Duration := envDuration("NETPROBE_Q2_DURATION", 5*time.Minute)
	q2Interval := envDuration("NETPROBE_Q2_INTERVAL", 20*time.Second)
	if q2Interval <= 0 {
		q2Interval = time.Second
	}
	opts := probeDialOptions()

	startupDeadline := time.Now().Add(120 * time.Second)
	var (
		conn         *grpc.ClientConn
		stream       grpc.ClientStream
		streamCancel context.CancelFunc
	)
	for {
		q1.Attempts++
		attemptStart := time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		timer := time.AfterFunc(30*time.Second, cancel)

		c, err := grpc.NewClient(probeTarget, opts...)
		if err == nil {
			var s grpc.ClientStream
			s, err = c.NewStream(ctx, bidiDesc, bidiMethod)
			if err == nil {
				err = sendWithTimeout(s, "q1-hello", 10*time.Second)
				if err == nil {
					var echo string
					echo, err = recvWithTimeout(s, 20*time.Second)
					if err == nil && echo == "echo:q1-hello" {
						q1.Echo = echo
					} else if err == nil {
						err = fmt.Errorf("unexpected echo %q", echo)
					}
				}
			}
			if err == nil {
				timer.Stop()
				conn, stream, streamCancel = c, s, cancel
				q1.Pass = true
				q1.ConnectedAt = now()
				q1.ConnectElapsedMs = time.Since(attemptStart).Milliseconds()
				break
			}
		}
		timer.Stop()
		cancel()
		if c != nil {
			_ = c.Close()
		}
		q1.AttemptErrors = append(q1.AttemptErrors, fmt.Sprintf("attempt %d: %v", q1.Attempts, err))
		if time.Now().After(startupDeadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}

	if !q1.Pass {
		q2.Skipped = true
		q2.Error = "Q1 did not establish a stream"
		return q1, q2
	}
	defer func() {
		streamCancel()
		_ = conn.Close()
	}()

	// Q2: one message each way every q2Interval for at least q2Duration. The
	// client keepalive above sends an HTTP/2 PING every 10s of inactivity.
	q2.StartTime = q1.ConnectedAt
	start := time.Now()
	seq := 0
	for {
		time.Sleep(q2Interval)
		seq++
		msg := "q2-seq=" + strconv.Itoa(seq)
		if err := sendWithTimeout(stream, msg, 15*time.Second); err != nil {
			q2.Error = "send " + msg + ": " + err.Error()
			break
		}
		q2.MessagesSent++
		echo, err := recvWithTimeout(stream, 30*time.Second)
		if err != nil {
			q2.Error = "recv " + msg + ": " + err.Error()
			break
		}
		if echo != "echo:"+msg {
			q2.Error = fmt.Sprintf("recv %s: unexpected echo %q", msg, echo)
			break
		}
		q2.MessagesReceived++
		if q2.FirstSeq == 0 {
			q2.FirstSeq = seq
		}
		q2.LastSeq = seq
		if time.Since(start) >= q2Duration {
			break
		}
	}

	elapsed := time.Since(start)
	q2.EndTime = now()
	q2.DurationSeconds = elapsed.Seconds()
	if q2.Error == "" {
		// Half-close and expect a clean EOF from the server.
		_ = stream.CloseSend()
		if _, err := recvWithTimeout(stream, 15*time.Second); err != nil && !errors.Is(err, io.EOF) {
			q2.Error = "after CloseSend: " + err.Error()
		}
	}
	q2.Pass = q2.Error == "" && elapsed >= q2Duration &&
		q2.MessagesSent == q2.MessagesReceived && q2.MessagesSent > 0
	return q1, q2
}

// probeDialOptions is the dial configuration shared by every stream the probe
// opens: cleartext h2c and a client keepalive PING every 10s with a 5s
// timeout. PermitWithoutStream keeps the pings flowing even when no stream is
// open.
func probeDialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	}
}

// runQ5 tests whether the egress gateway cuts an otherwise idle stream. After
// a first round-trip it sends no application message for NETPROBE_IDLE_QUIET
// (six minutes by default) while the client keepalive keeps pinging every
// 10s, then resumes request/response traffic until NETPROBE_IDLE_TOTAL (ten
// minutes by default) has passed. Any error ends the test as a cut and is
// reported with its time and gRPC status. A cut is a valid conclusion; only a
// test that cannot settle the question fails.
func runQ5() q5Result {
	res := q5Result{
		Target:                  probeTarget,
		KeepalivePingSeconds:    10,
		KeepaliveTimeoutSeconds: 5,
	}
	quiet := envDuration("NETPROBE_IDLE_QUIET", 6*time.Minute)
	total := envDuration("NETPROBE_IDLE_TOTAL", 10*time.Minute)
	if quiet < 0 {
		quiet = 0
	}
	if total <= quiet {
		total = quiet + time.Second
	}
	res.QuietSeconds = quiet.Seconds()
	res.ActiveSeconds = (total - quiet).Seconds()
	res.TotalSeconds = total.Seconds()

	conn, stream, cancel, err := openEchoStream("q5", 60*time.Second)
	if err != nil {
		res.Error = "open stream: " + err.Error()
		return res
	}
	defer func() {
		cancel()
		_ = conn.Close()
	}()

	start := time.Now()
	recvCh := streamReader(stream)

	// Quiet phase: application messages stop. A receive timeout is the normal
	// end of the phase; an error is the cut this phase is looking for.
	quietTimer := time.NewTimer(quiet)
	defer quietTimer.Stop()
	select {
	case rr := <-recvCh:
		if rr.err != nil {
			return concludeQ5Cut(res, start, quiet, rr.err)
		}
		res.Error = fmt.Sprintf("unexpected message during quiet phase: %q", rr.value)
		return res
	case <-quietTimer.C:
	}

	// Active phase: resume traffic until the total duration has elapsed.
	interval := envDuration("NETPROBE_IDLE_ACTIVE_INTERVAL", 20*time.Second)
	if interval <= 0 {
		interval = time.Second
	}
	for time.Since(start) < total {
		msg := "q5-resume=" + strconv.Itoa(res.MessagesAfterIdle+1)
		if err := sendWithTimeout(stream, msg, 15*time.Second); err != nil {
			return concludeQ5Cut(res, start, quiet, err)
		}
		res.MessagesAfterIdle++
		echo, err := recvFrom(recvCh, 30*time.Second)
		if err != nil {
			return concludeQ5Cut(res, start, quiet, err)
		}
		if echo != "echo:"+msg {
			res.Error = fmt.Sprintf("unexpected echo %q for %q", echo, msg)
			return res
		}
		wait := interval
		if remaining := total - time.Since(start); remaining < wait {
			wait = remaining
		}
		if wait > 0 {
			time.Sleep(wait)
		}
	}

	res.Survived = true
	res.DurationSeconds = time.Since(start).Seconds()
	if res.MessagesAfterIdle == 0 {
		res.Error = "no message round-trip after the quiet phase"
		return res
	}
	res.Pass = true
	return res
}

// concludeQ5Cut records a stream failure as the answer to Q5: how long the
// stream lasted, in which phase it died, the gRPC status, and which side saw
// it first. The server's own close time is fetched over a fresh connection,
// because the cut may have killed the connection that carried the stream.
func concludeQ5Cut(res q5Result, start time.Time, quiet time.Duration, err error) q5Result {
	elapsed := time.Since(start)
	res.Cut = true
	res.DurationSeconds = elapsed.Seconds()
	res.CutAfterSeconds = elapsed.Seconds()
	if elapsed < quiet {
		res.CutPhase = "quiet"
	} else {
		res.CutPhase = "active"
	}
	res.ClientDetectedAt = now()

	if errors.Is(err, io.EOF) {
		res.GRPCStatusCode = "OK"
		res.GRPCCode = 0
		res.GRPCMessage = "server ended the stream (io.EOF)"
	} else {
		st := status.Convert(err)
		res.GRPCStatusCode = st.Code().String()
		res.GRPCCode = uint32(st.Code())
		res.GRPCMessage = st.Message()
		if res.GRPCMessage == "" {
			res.GRPCMessage = err.Error()
		}
	}

	rep, rerr := fetchServerReport()
	if rerr != nil {
		res.ServerReportError = rerr.Error()
		res.Pass = true
		return res
	}
	if rep.LastClose.At == "" {
		res.ServerReportError = "server reported no closed stream"
		res.Pass = true
		return res
	}
	res.ServerDetectedAt = rep.LastClose.At
	serverAt, perr := time.Parse(time.RFC3339Nano, rep.LastClose.At)
	clientAt, cerr := time.Parse(time.RFC3339Nano, res.ClientDetectedAt)
	if perr != nil || cerr != nil {
		res.ServerReportError = "parse close timestamps: " + fmt.Sprint(perr, cerr)
		res.Pass = true
		return res
	}
	// The two timestamps come from different hosts, so compare them with a
	// one-second tolerance and print both for the operator to check.
	delta := clientAt.Sub(serverAt)
	res.DetectionDeltaMs = delta.Milliseconds()
	switch {
	case delta > time.Second:
		res.DetectedBy = "server"
	case delta < -time.Second:
		res.DetectedBy = "client"
	default:
		res.DetectedBy = "simultaneous"
	}
	res.Pass = true
	return res
}

// fetchServerReport asks the development-host server when it saw the stream
// close. It retries because a cut may briefly make the Service unreachable.
func fetchServerReport() (serverStreamReport, error) {
	var rep serverStreamReport
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, err := grpc.NewClient(probeTarget, probeDialOptions()...)
		if err == nil {
			var out wrapperspb.StringValue
			err = conn.Invoke(ctx, reportMethod, wrapperspb.String(""), &out)
			_ = conn.Close()
			if err == nil {
				cancel()
				if uerr := json.Unmarshal([]byte(out.GetValue()), &rep); uerr != nil {
					return rep, fmt.Errorf("decode server report: %w", uerr)
				}
				return rep, nil
			}
		}
		cancel()
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	return rep, lastErr
}

// openEchoStream dials the target, opens a Bidi stream and completes one
// round-trip, retrying until the timeout expires.
func openEchoStream(tag string, timeout time.Duration) (*grpc.ClientConn, grpc.ClientStream, context.CancelFunc, error) {
	opts := probeDialOptions()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ctx, cancel := context.WithCancel(context.Background())
		c, err := grpc.NewClient(probeTarget, opts...)
		if err == nil {
			var s grpc.ClientStream
			s, err = c.NewStream(ctx, bidiDesc, bidiMethod)
			if err == nil {
				hello := tag + "-hello"
				if serr := sendWithTimeout(s, hello, 10*time.Second); serr == nil {
					echo, rerr := recvWithTimeout(s, 20*time.Second)
					if rerr == nil && echo == "echo:"+hello {
						return c, s, cancel, nil
					} else if rerr == nil {
						err = fmt.Errorf("unexpected echo %q", echo)
					} else {
						err = rerr
					}
				} else {
					err = serr
				}
			}
			_ = c.Close()
		}
		cancel()
		lastErr = err
		if time.Now().After(deadline) {
			return nil, nil, nil, lastErr
		}
		time.Sleep(5 * time.Second)
	}
}

type recvResult struct {
	value string
	err   error
}

// streamReader reads messages continuously. Keeping a single reader goroutine
// avoids calling RecvMsg concurrently when a timed wait gives up: the pending
// read stays with the reader and delivers the next message when it arrives.
func streamReader(stream grpc.ClientStream) <-chan recvResult {
	ch := make(chan recvResult, 16)
	go func() {
		for {
			m := new(wrapperspb.StringValue)
			err := stream.RecvMsg(m)
			ch <- recvResult{value: m.GetValue(), err: err}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

func recvFrom(ch <-chan recvResult, d time.Duration) (string, error) {
	select {
	case r := <-ch:
		return r.value, r.err
	case <-time.After(d):
		return "", fmt.Errorf("recv timed out after %s", d)
	}
}

func sendWithTimeout(stream grpc.ClientStream, value string, d time.Duration) error {
	ch := make(chan error, 1)
	go func() {
		ch <- stream.SendMsg(&wrapperspb.StringValue{Value: value})
	}()
	select {
	case err := <-ch:
		return err
	case <-time.After(d):
		return fmt.Errorf("send timed out after %s", d)
	}
}

func recvWithTimeout(stream grpc.ClientStream, d time.Duration) (string, error) {
	type result struct {
		msg *wrapperspb.StringValue
		err error
	}
	ch := make(chan result, 1)
	go func() {
		m := new(wrapperspb.StringValue)
		ch <- result{msg: m, err: stream.RecvMsg(m)}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return "", r.err
		}
		return r.msg.GetValue(), nil
	case <-time.After(d):
		return "", fmt.Errorf("recv timed out after %s", d)
	}
}

// runQ3 retries the TLS probe for up to 90s. A new actor's EgressPolicy takes
// a few seconds to reach the DNS relay and the gateway's policy cache, and a
// connection attempted before that is refused; a retry separates that startup
// latency from a rule that does not work at all.
func runQ3() q3Result {
	deadline := time.Now().Add(90 * time.Second)
	var transient []string
	for attempt := 1; ; attempt++ {
		res := runQ3Once()
		res.Attempts = attempt
		if res.Pass {
			res.TransientErrors = transient
			return res
		}
		transient = append(transient, fmt.Sprintf("attempt %d: %s", attempt, res.Error))
		if time.Now().After(deadline) {
			res.TransientErrors = transient
			return res
		}
		time.Sleep(5 * time.Second)
	}
}

// runQ3Once completes a TLS handshake to api.anthropic.com:443 through the
// tls_passthrough rule, then sends one request with no credentials and reads
// the status line. No request body and no API header are sent, so no quota is
// consumed.
func runQ3Once() q3Result {
	const host = "api.anthropic.com"
	res := q3Result{Host: host + ":443"}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	var dialer net.Dialer
	tcpStart := time.Now()
	raw, err := dialer.DialContext(ctx, "tcp", res.Host)
	if err != nil {
		res.Error = "tcp connect: " + err.Error()
		return res
	}
	defer raw.Close()
	res.TCPConnectMs = time.Since(tcpStart).Milliseconds()

	tlsStart := time.Now()
	tconn := tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err := tconn.HandshakeContext(ctx); err != nil {
		res.Error = "tls handshake: " + err.Error()
		return res
	}
	res.HandshakeMs = time.Since(tlsStart).Milliseconds()

	state := tconn.ConnectionState()
	res.TLSVersion = tls.VersionName(state.Version)
	res.CipherSuite = tls.CipherSuiteName(state.CipherSuite)
	if len(state.PeerCertificates) > 0 {
		leaf := state.PeerCertificates[0]
		res.PeerSubject = leaf.Subject.String()
		res.PeerIssuer = leaf.Issuer.String()
		res.PeerNotAfter = leaf.NotAfter.UTC().Format(time.RFC3339)
	}

	_ = raw.SetDeadline(time.Now().Add(20 * time.Second))
	request := "GET / HTTP/1.1\r\nHost: " + host +
		"\r\nUser-Agent: castworks-netprobe/1\r\nAccept: */*\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(tconn, request); err != nil {
		res.Error = "http write: " + err.Error()
		return res
	}
	line, err := bufio.NewReader(tconn).ReadString('\n')
	if err != nil {
		res.Error = "http read: " + err.Error()
		return res
	}
	res.HTTPStatus = strings.TrimSpace(line)
	res.Pass = strings.HasPrefix(res.HTTPStatus, "HTTP/")
	return res
}

// runQ4 checks that a host absent from the EgressPolicy is refused. A failure
// to resolve, or a refused connection, both count: the Substrate DNS relay
// answers only policy hostnames, and the gateway denies the rest.
func runQ4() q4Result {
	const host = "example.com"
	res := q4Result{Host: host + ":443", PolicyNote: "host is not listed in the EgressPolicy"}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	cancel()
	if err != nil {
		res.LookupErr = err.Error()
	}
	res.Addrs = addrs

	if len(addrs) > 0 {
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		var dialer net.Dialer
		c, derr := dialer.DialContext(dctx, "tcp", net.JoinHostPort(addrs[0], "443"))
		dcancel()
		if derr != nil {
			res.DialErr = derr.Error()
		} else {
			res.DialOK = true
			_ = c.Close()
		}
	}
	res.ElapsedMs = time.Since(start).Milliseconds()
	res.Pass = res.LookupErr != "" || (res.DialErr != "" && !res.DialOK)
	return res
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("invalid %s=%q, using %s", name, v, fallback)
		return fallback
	}
	return d
}

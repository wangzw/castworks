package substrate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	authv1 "k8s.io/api/authentication/v1"
	certsv1 "k8s.io/api/certificates/v1"
	certsv1beta1 "k8s.io/api/certificates/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

// newAteAPIClient is the entry the provider constructor calls; pinning its
// signature here keeps the two work items' agreement explicit.
var _ func(context.Context, ateAPIConnConfig) (ateapi.ControlClient, func() error, error) = newAteAPIClient

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestTokenSourceReusesAndRenewsToken(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	var mints atomic.Int64
	var mintFails atomic.Bool
	mint := func(context.Context) (string, time.Time, error) {
		if mintFails.Load() {
			return "", time.Time{}, errors.New("mint failed")
		}
		n := mints.Add(1)
		return fmt.Sprintf("token-%d", n), clock.Now().Add(time.Hour), nil
	}
	source := &tokenSource{mint: mint, now: clock.Now, ttl: time.Hour}

	if err := source.refresh(context.Background()); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	assertToken(t, source, "token-1", 1, &mints)

	clock.Advance(29 * time.Minute)
	assertToken(t, source, "token-1", 1, &mints)

	clock.Advance(2 * time.Minute)
	assertToken(t, source, "token-2", 2, &mints)

	// A failed renewal keeps the old token while it has not expired.
	mintFails.Store(true)
	clock.Advance(31 * time.Minute)
	assertToken(t, source, "token-2", 2, &mints)

	// Once the old token has expired, the failure is returned.
	clock.Advance(31 * time.Minute)
	if _, err := source.get(context.Background()); err == nil {
		t.Fatal("get succeeded after the token expired and renewal failed")
	}
}

func assertToken(t *testing.T, source *tokenSource, want string, wantMints int64, mints *atomic.Int64) {
	t.Helper()
	token, err := source.get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if token != want {
		t.Fatalf("get returned %q, want %q", token, want)
	}
	if got := mints.Load(); got != wantMints {
		t.Fatalf("minted %d tokens, want %d", got, wantMints)
	}
}

func TestTokenSourceRunRenewsUntilCanceled(t *testing.T) {
	var mints atomic.Int64
	source := &tokenSource{
		mint: func(context.Context) (string, time.Time, error) {
			n := mints.Add(1)
			return fmt.Sprintf("token-%d", n), time.Now().Add(80 * time.Millisecond), nil
		},
		now: time.Now,
		ttl: 80 * time.Millisecond,
	}
	if err := source.refresh(context.Background()); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		source.run(ctx)
	}()

	waitFor(t, 5*time.Second, func() bool { return mints.Load() >= 3 })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop after its context was canceled")
	}
	after := mints.Load()
	time.Sleep(200 * time.Millisecond)
	if got := mints.Load(); got != after {
		t.Fatalf("token was renewed after the run context was canceled: %d -> %d", after, got)
	}
}

func TestServerTLSConfigSelectsMatchingBundle(t *testing.T) {
	_, caPEM := newTestServerCertificate(t)
	otherBundle := testBundle{
		name:   "other",
		signer: clusterTrustBundleSigner,
		labels: map[string]string{"podcert.ate.dev/canarying": "live"},
		pem:    caPEM,
	}

	tests := []struct {
		name    string
		beta    bool
		bundles []testBundle
		wantErr bool
	}{
		{
			name:    "v1 matching label and signer",
			bundles: []testBundle{matchingBundle(caPEM)},
		},
		{
			name: "wrong signer is ignored",
			bundles: []testBundle{{
				name:   "wrong-signer",
				signer: "other.example.com/identity",
				labels: map[string]string{"podcert.ate.dev/canarying": "live"},
				pem:    caPEM,
			}},
			wantErr: true,
		},
		{
			name: "wrong label is ignored",
			bundles: []testBundle{{
				name:   "wrong-label",
				signer: clusterTrustBundleSigner,
				labels: map[string]string{"podcert.ate.dev/canarying": "staging"},
				pem:    caPEM,
			}},
			wantErr: true,
		},
		{
			name:    "v1beta1 fallback",
			beta:    true,
			bundles: []testBundle{matchingBundle(caPEM)},
		},
		{
			name:    "no bundles",
			bundles: nil,
			wantErr: true,
		},
		{
			name:    "one matching among several",
			bundles: []testBundle{otherBundle, {name: "wrong-signer", signer: "x", labels: nil, pem: caPEM}, matchingBundle(caPEM)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fakeClientset(t, tt.beta, tt.bundles)
			cfg, err := serverTLSConfig(context.Background(), clientset, ateServerName)
			if tt.wantErr {
				if err == nil {
					t.Fatal("serverTLSConfig succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("serverTLSConfig: %v", err)
			}
			if cfg.MinVersion != tls.VersionTLS13 {
				t.Fatalf("MinVersion = %x, want TLS 1.3", cfg.MinVersion)
			}
			if cfg.ServerName != ateServerName {
				t.Fatalf("ServerName = %q, want %q", cfg.ServerName, ateServerName)
			}
			if cfg.RootCAs == nil {
				t.Fatal("RootCAs is nil")
			}
		})
	}
}

func TestServerTLSConfigErrorsWhenNoClusterTrustBundleAPI(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	if _, err := serverTLSConfig(context.Background(), clientset, ateServerName); err == nil {
		t.Fatal("serverTLSConfig succeeded without a ClusterTrustBundle API")
	}
}

func TestAteAPIClientRebuildsForwardOnUnavailable(t *testing.T) {
	serverCert, caPEM := newTestServerCertificate(t)

	tests := []struct {
		name      string
		failFirst int
		wantErr   bool
	}{
		{name: "one Unavailable is retried after a rebuild", failFirst: 1},
		{name: "two Unavailable are returned", failFirst: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			impl := &fakeControlServer{failFirst: tt.failFirst}
			port, stopServer := startFakeControlServer(t, serverCert, impl)
			defer stopServer()

			clientset := fakeClientset(t, false, []testBundle{matchingBundle(caPEM)})
			recorder := addTokenReactor(clientset, time.Hour)
			var forwards atomic.Int64
			client, closeClient, err := newAteAPIClientWithDeps(
				context.Background(),
				ateAPIConnConfig{ServiceAccount: "castworks-system/sandboxprovider"},
				testDeps(clientset, port, &forwards, time.Hour),
			)
			if err != nil {
				t.Fatalf("newAteAPIClientWithDeps: %v", err)
			}
			defer func() { _ = closeClient() }()

			_, err = client.GetActor(context.Background(), &ateapi.GetActorRequest{})
			if tt.wantErr {
				if status.Code(err) != codes.Unavailable {
					t.Fatalf("GetActor error = %v, want Unavailable", err)
				}
			} else if err != nil {
				t.Fatalf("GetActor: %v", err)
			}

			if got := forwards.Load(); got != 2 {
				t.Fatalf("port-forward started %d times, want 2", got)
			}
			if got := impl.callCount(); got != 2 {
				t.Fatalf("server saw %d calls, want 2", got)
			}
			if got := recorder.count(); got != 1 {
				t.Fatalf("minted %d tokens, want 1", got)
			}
			if got := impl.lastAuth(); got != "Bearer token-1" {
				t.Fatalf("authorization header = %q, want %q", got, "Bearer token-1")
			}
		})
	}
}

func TestAteAPIClientStopsRetiredForwards(t *testing.T) {
	serverCert, caPEM := newTestServerCertificate(t)
	impl := &fakeControlServer{failFirst: -1}
	port, stopServer := startFakeControlServer(t, serverCert, impl)
	defer stopServer()

	clientset := fakeClientset(t, false, []testBundle{matchingBundle(caPEM)})
	addTokenReactor(clientset, time.Hour)

	var started, stopped atomic.Int64
	deps := baseTestDeps(clientset)
	deps.startForward = func(context.Context, *rest.Config, kubernetes.Interface) (int, func(), error) {
		started.Add(1)
		return port, func() { stopped.Add(1) }, nil
	}

	client, closeClient, err := newAteAPIClientWithDeps(
		context.Background(),
		ateAPIConnConfig{ServiceAccount: "castworks-system/sandboxprovider"},
		deps,
	)
	if err != nil {
		t.Fatalf("newAteAPIClientWithDeps: %v", err)
	}
	defer func() { _ = closeClient() }()

	for i := range 5 {
		if _, err := client.GetActor(context.Background(), &ateapi.GetActorRequest{}); status.Code(err) != codes.Unavailable {
			t.Fatalf("call %d error = %v, want Unavailable", i+1, err)
		}
	}
	if live := started.Load() - stopped.Load(); live > 2 {
		t.Fatalf("%d forwards still live after 5 calls (started %d, stopped %d), want at most 2",
			live, started.Load(), stopped.Load())
	}
}

// gatedForwardServer keeps the first GetActor in flight until the test releases
// it and fails the second with Unavailable, so a test can retire the forward
// that still carries one call.
type gatedForwardServer struct {
	ateapi.UnimplementedControlServer

	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (s *gatedForwardServer) GetActor(ctx context.Context, _ *ateapi.GetActorRequest) (*ateapi.Actor, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()

	switch call {
	case 1:
		close(s.entered)
		select {
		case <-s.release:
			return &ateapi.Actor{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case 2:
		return nil, status.Error(codes.Unavailable, "forward replaced")
	default:
		return &ateapi.Actor{}, nil
	}
}

func TestAteAPIClientLastReleaseStopsRetiredForward(t *testing.T) {
	serverCert, caPEM := newTestServerCertificate(t)
	impl := &gatedForwardServer{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	port, stopServer := startControlServer(t, serverCert, impl)
	defer stopServer()

	clientset := fakeClientset(t, false, []testBundle{matchingBundle(caPEM)})
	addTokenReactor(clientset, time.Hour)

	var started, stoppedOld, stoppedNew atomic.Int64
	deps := baseTestDeps(clientset)
	deps.startForward = func(context.Context, *rest.Config, kubernetes.Interface) (int, func(), error) {
		if started.Add(1) == 1 {
			return port, func() { stoppedOld.Add(1) }, nil
		}
		return port, func() { stoppedNew.Add(1) }, nil
	}

	client, closeClient, err := newAteAPIClientWithDeps(
		context.Background(),
		ateAPIConnConfig{ServiceAccount: "castworks-system/sandboxprovider"},
		deps,
	)
	if err != nil {
		t.Fatalf("newAteAPIClientWithDeps: %v", err)
	}
	var releaseOnce sync.Once
	releaseInflight := func() { releaseOnce.Do(func() { close(impl.release) }) }
	defer func() {
		releaseInflight()
		_ = closeClient()
	}()

	// The first call stays in flight on the current forward.
	inflight := make(chan error, 1)
	go func() {
		_, err := client.GetActor(context.Background(), &ateapi.GetActorRequest{})
		inflight <- err
	}()
	<-impl.entered

	// A second call on the same generation fails Unavailable and rebuilds the
	// forward while the first call is still using the old one.
	if _, err := client.GetActor(context.Background(), &ateapi.GetActorRequest{}); err != nil {
		t.Fatalf("GetActor that rebuilt the forward: %v", err)
	}
	if got := started.Load(); got != 2 {
		t.Fatalf("port-forward started %d times, want 2", got)
	}
	if got := stoppedOld.Load(); got != 0 {
		t.Fatalf("old forward stopped %d times while an RPC was still in flight, want 0", got)
	}

	releaseInflight()
	if err := <-inflight; err != nil {
		t.Fatalf("in-flight GetActor: %v", err)
	}
	// The last release on the retired forward stops it exactly once.
	if got := stoppedOld.Load(); got != 1 {
		t.Fatalf("old forward stopped %d times after its last release, want exactly 1", got)
	}
	if got := stoppedNew.Load(); got != 0 {
		t.Fatalf("new forward stopped %d times before Close, want 0", got)
	}

	if err := closeClient(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := stoppedOld.Load(); got != 1 {
		t.Fatalf("old forward stopped %d times after Close, want it to stay at 1", got)
	}
	if got := stoppedNew.Load(); got != 1 {
		t.Fatalf("new forward stopped %d times after Close, want 1", got)
	}
}

func TestAteAPIClientRetriesOnRebuiltForward(t *testing.T) {
	serverCert, caPEM := newTestServerCertificate(t)
	failing := &fakeControlServer{failFirst: -1}
	failingPort, stopFailing := startFakeControlServer(t, serverCert, failing)
	defer stopFailing()
	healthy := &fakeControlServer{}
	healthyPort, stopHealthy := startFakeControlServer(t, serverCert, healthy)
	defer stopHealthy()

	clientset := fakeClientset(t, false, []testBundle{matchingBundle(caPEM)})
	addTokenReactor(clientset, time.Hour)

	var forwards atomic.Int64
	deps := baseTestDeps(clientset)
	deps.startForward = func(context.Context, *rest.Config, kubernetes.Interface) (int, func(), error) {
		if forwards.Add(1) == 1 {
			return failingPort, func() {}, nil
		}
		return healthyPort, func() {}, nil
	}

	client, closeClient, err := newAteAPIClientWithDeps(
		context.Background(),
		ateAPIConnConfig{ServiceAccount: "castworks-system/sandboxprovider"},
		deps,
	)
	if err != nil {
		t.Fatalf("newAteAPIClientWithDeps: %v", err)
	}
	defer func() { _ = closeClient() }()

	if _, err := client.GetActor(context.Background(), &ateapi.GetActorRequest{}); err != nil {
		t.Fatalf("GetActor: %v", err)
	}
	if got := failing.callCount(); got != 1 {
		t.Fatalf("old forward saw %d calls, want 1", got)
	}
	if got := healthy.callCount(); got != 1 {
		t.Fatalf("new forward saw %d calls, want 1", got)
	}

	// Later calls keep using the replacement forward.
	if _, err := client.GetActor(context.Background(), &ateapi.GetActorRequest{}); err != nil {
		t.Fatalf("second GetActor: %v", err)
	}
	if got := failing.callCount(); got != 1 {
		t.Fatalf("old forward saw %d calls, want 1", got)
	}
	if got := healthy.callCount(); got != 2 {
		t.Fatalf("new forward saw %d calls, want 2", got)
	}
	if got := forwards.Load(); got != 2 {
		t.Fatalf("port-forward started %d times, want 2", got)
	}
}

func TestAteAPIClientRebuildsOncePerGeneration(t *testing.T) {
	const calls = 8
	serverCert, caPEM := newTestServerCertificate(t)

	arrived := make(chan struct{}, calls)
	release := make(chan struct{})
	failing := &fakeControlServer{failFirst: -1, gate: func() {
		arrived <- struct{}{}
		<-release
	}}
	failingPort, stopFailing := startFakeControlServer(t, serverCert, failing)
	defer stopFailing()
	healthy := &fakeControlServer{}
	healthyPort, stopHealthy := startFakeControlServer(t, serverCert, healthy)
	defer stopHealthy()

	clientset := fakeClientset(t, false, []testBundle{matchingBundle(caPEM)})
	addTokenReactor(clientset, time.Hour)

	var forwards atomic.Int64
	deps := baseTestDeps(clientset)
	deps.startForward = func(context.Context, *rest.Config, kubernetes.Interface) (int, func(), error) {
		if forwards.Add(1) == 1 {
			return failingPort, func() {}, nil
		}
		return healthyPort, func() {}, nil
	}

	client, closeClient, err := newAteAPIClientWithDeps(
		context.Background(),
		ateAPIConnConfig{ServiceAccount: "castworks-system/sandboxprovider"},
		deps,
	)
	if err != nil {
		t.Fatalf("newAteAPIClientWithDeps: %v", err)
	}
	defer func() { _ = closeClient() }()

	// All callers reach the first forward before any of them returns, so they
	// all fail on the same generation and race into the rebuild.
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := client.GetActor(context.Background(), &ateapi.GetActorRequest{}); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	for range calls {
		<-arrived
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("GetActor: %v", err)
	}

	if got := forwards.Load(); got != 2 {
		t.Fatalf("port-forward started %d times, want 2 (one rebuild shared by every caller)", got)
	}
	if got := healthy.callCount(); got != calls {
		t.Fatalf("rebuilt forward saw %d calls, want %d", got, calls)
	}
}

func TestAteAPIClientRebuildWaitsOnCallerContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{
			name: "deadline",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 100*time.Millisecond)
			},
		},
		{
			name: "canceled",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(50*time.Millisecond, cancel)
				return ctx, cancel
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverCert, caPEM := newTestServerCertificate(t)
			impl := &fakeControlServer{failFirst: -1}
			port, stopServer := startFakeControlServer(t, serverCert, impl)
			defer stopServer()

			clientset := fakeClientset(t, false, []testBundle{matchingBundle(caPEM)})
			addTokenReactor(clientset, time.Hour)

			entered := make(chan struct{})
			release := make(chan struct{})
			var enteredOnce, releaseOnce sync.Once
			releaseForward := func() { releaseOnce.Do(func() { close(release) }) }

			var forwards atomic.Int64
			deps := baseTestDeps(clientset)
			deps.startForward = func(ctx context.Context, _ *rest.Config, _ kubernetes.Interface) (int, func(), error) {
				if forwards.Add(1) == 1 {
					return port, func() {}, nil
				}
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-ctx.Done():
					return 0, nil, ctx.Err()
				}
				return port, func() {}, nil
			}

			client, closeClient, err := newAteAPIClientWithDeps(
				context.Background(),
				ateAPIConnConfig{ServiceAccount: "castworks-system/sandboxprovider"},
				deps,
			)
			if err != nil {
				t.Fatalf("newAteAPIClientWithDeps: %v", err)
			}
			defer func() {
				releaseForward()
				_ = closeClient()
			}()

			// The first RPC returns Unavailable and blocks in its rebuild.
			blocked := make(chan struct{})
			go func() {
				defer close(blocked)
				_, _ = client.GetActor(context.Background(), &ateapi.GetActorRequest{})
			}()
			<-entered

			// Release the blocked rebuild even if the code under test ignores
			// the caller's context.
			safety := time.AfterFunc(2*time.Second, releaseForward)
			defer safety.Stop()

			ctx, cancel := tt.ctx()
			defer cancel()
			start := time.Now()
			_, err = client.GetActor(ctx, &ateapi.GetActorRequest{})
			elapsed := time.Since(start)
			releaseForward()

			if elapsed > time.Second {
				t.Fatalf("call returned after %v, want it to honor its context", elapsed)
			}
			if err == nil {
				t.Fatal("call succeeded, want an error")
			}
			<-blocked
		})
	}
}

func TestAteAPIClientRebuildTimesOut(t *testing.T) {
	serverCert, caPEM := newTestServerCertificate(t)
	impl := &fakeControlServer{failFirst: -1}
	port, stopServer := startFakeControlServer(t, serverCert, impl)
	defer stopServer()

	clientset := fakeClientset(t, false, []testBundle{matchingBundle(caPEM)})
	addTokenReactor(clientset, time.Hour)

	var forwards atomic.Int64
	deps := baseTestDeps(clientset)
	deps.rebuildTimeout = 100 * time.Millisecond
	deps.startForward = func(ctx context.Context, _ *rest.Config, _ kubernetes.Interface) (int, func(), error) {
		if forwards.Add(1) == 1 {
			return port, func() {}, nil
		}
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-time.After(2 * time.Second):
			return port, func() {}, nil
		}
	}

	client, closeClient, err := newAteAPIClientWithDeps(
		context.Background(),
		ateAPIConnConfig{ServiceAccount: "castworks-system/sandboxprovider"},
		deps,
	)
	if err != nil {
		t.Fatalf("newAteAPIClientWithDeps: %v", err)
	}
	defer func() { _ = closeClient() }()

	start := time.Now()
	_, err = client.GetActor(context.Background(), &ateapi.GetActorRequest{})
	elapsed := time.Since(start)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("GetActor error = %v, want Unavailable", err)
	}
	if elapsed > time.Second {
		t.Fatalf("rebuild took %v, want it to honor the rebuild timeout", elapsed)
	}
}

func TestAteAPIClientMintsTokenForServiceAccount(t *testing.T) {
	serverCert, caPEM := newTestServerCertificate(t)
	impl := &fakeControlServer{}
	port, stopServer := startFakeControlServer(t, serverCert, impl)
	defer stopServer()

	clientset := fakeClientset(t, false, []testBundle{matchingBundle(caPEM)})
	recorder := addTokenReactor(clientset, time.Hour)

	var forwards atomic.Int64
	client, closeClient, err := newAteAPIClientWithDeps(
		context.Background(),
		ateAPIConnConfig{ServiceAccount: "castworks-system/sandboxprovider"},
		testDeps(clientset, port, &forwards, time.Hour),
	)
	if err != nil {
		t.Fatalf("newAteAPIClientWithDeps: %v", err)
	}
	defer func() { _ = closeClient() }()

	if _, err := client.GetActor(context.Background(), &ateapi.GetActorRequest{}); err != nil {
		t.Fatalf("GetActor: %v", err)
	}

	requests := recorder.all()
	if len(requests) != 1 {
		t.Fatalf("recorded %d token requests, want 1", len(requests))
	}
	request := requests[0]
	if request.namespace != "castworks-system" || request.name != "sandboxprovider" {
		t.Fatalf("token requested for %s/%s, want castworks-system/sandboxprovider", request.namespace, request.name)
	}
	if len(request.audiences) != 1 || request.audiences[0] != ateTokenAudience {
		t.Fatalf("token audiences = %v, want [%s]", request.audiences, ateTokenAudience)
	}
	if want := int64(time.Hour / time.Second); request.expirationSeconds != want {
		t.Fatalf("token expirationSeconds = %d, want %d", request.expirationSeconds, want)
	}
	if got := recorder.count(); got != 1 {
		t.Fatalf("minted %d tokens, want 1", got)
	}
	if got := impl.callCount(); got != 1 {
		t.Fatalf("server saw %d calls, want 1", got)
	}
}

func TestAteAPIClientCloseWaitsForTokenRenewal(t *testing.T) {
	serverCert, caPEM := newTestServerCertificate(t)
	impl := &fakeControlServer{}
	port, stopServer := startFakeControlServer(t, serverCert, impl)
	defer stopServer()

	clientset := fakeClientset(t, false, []testBundle{matchingBundle(caPEM)})

	const ttl = 50 * time.Millisecond
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseMint := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseMint()
	var mints atomic.Int64
	deps := baseTestDeps(clientset)
	deps.tokenTTL = ttl
	deps.startForward = func(context.Context, *rest.Config, kubernetes.Interface) (int, func(), error) {
		return port, func() {}, nil
	}
	deps.mintToken = func(context.Context, kubernetes.Interface, string, string, string, time.Duration) (string, time.Time, error) {
		n := mints.Add(1)
		if n > 1 {
			enteredOnce.Do(func() { close(entered) })
			<-release // deliberately ignores the canceled context
		}
		return fmt.Sprintf("token-%d", n), time.Now().Add(ttl), nil
	}

	_, closeClient, err := newAteAPIClientWithDeps(
		context.Background(),
		ateAPIConnConfig{ServiceAccount: "castworks-system/sandboxprovider"},
		deps,
	)
	if err != nil {
		t.Fatalf("newAteAPIClientWithDeps: %v", err)
	}

	<-entered
	closed := make(chan error, 1)
	go func() { closed <- closeClient() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) before the renewal goroutine exited", err)
	case <-time.After(100 * time.Millisecond):
	}

	releaseMint()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the renewal mint finished")
	}
}

func TestTokenSourceGetReturnsValidTokenWhileMintInProgress(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	mintEntered := make(chan struct{})
	mintRelease := make(chan struct{})
	var releaseOnce sync.Once
	source := &tokenSource{
		token:     "old",
		expiresAt: clock.Now().Add(time.Hour),
		renewAt:   clock.Now().Add(-time.Minute), // renewal is due
		now:       clock.Now,
		ttl:       time.Hour,
		mint: func(context.Context) (string, time.Time, error) {
			close(mintEntered)
			<-mintRelease
			return "new", clock.Now().Add(time.Hour), nil
		},
	}

	minted := make(chan error, 1)
	go func() {
		_, err := source.get(context.Background())
		minted <- err
	}()
	<-mintEntered

	// A mint is in progress, but the old token is still valid: get must not
	// wait for the mint.
	releaseMint := func() { releaseOnce.Do(func() { close(mintRelease) }) }
	defer releaseMint()
	safety := time.AfterFunc(2*time.Second, releaseMint)
	defer safety.Stop()

	start := time.Now()
	token, err := source.get(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("get while minting: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("get blocked for %v while a mint was in progress", elapsed)
	}
	if token != "old" {
		t.Fatalf("get while minting returned %q, want the still-valid old token", token)
	}

	releaseMint()
	if err := <-minted; err != nil {
		t.Fatalf("minting get: %v", err)
	}
	source.mu.Lock()
	got := source.token
	source.mu.Unlock()
	if got != "new" {
		t.Fatalf("token after the mint = %q, want new", got)
	}
}

func TestTokenSourceMintSurvivesCallerCancellation(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	mintEntered := make(chan struct{}, 1)
	mintCanceled := make(chan error, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseMint := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseMint()

	source := &tokenSource{
		token:       "expired",
		expiresAt:   clock.Now().Add(-time.Minute),
		renewAt:     clock.Now().Add(-time.Minute),
		now:         clock.Now,
		ttl:         time.Hour,
		background:  context.Background(),
		mintTimeout: time.Minute,
		mint: func(ctx context.Context) (string, time.Time, error) {
			select {
			case mintEntered <- struct{}{}:
			default:
			}
			select {
			case <-release:
				return "new", clock.Now().Add(time.Hour), nil
			case <-ctx.Done():
				mintCanceled <- ctx.Err()
				return "", time.Time{}, ctx.Err()
			}
		},
	}

	const callers = 3
	joined := make(chan struct{}, callers)
	source.onWait = func() { joined <- struct{}{} }

	// The first caller starts the shared mint and is then canceled.
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := source.get(firstCtx)
		first <- err
	}()
	select {
	case <-mintEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the mint did not start")
	}

	type result struct {
		token string
		err   error
	}
	results := make(chan result, callers)
	for range callers {
		go func() {
			token, err := source.get(context.Background())
			results <- result{token: token, err: err}
		}()
	}
	// Wait until every other caller has joined the shared refresh. Each has
	// captured the refresh channel, so it will observe this mint's result.
	for range callers {
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Fatal("a caller did not join the shared refresh")
		}
	}

	cancelFirst()
	select {
	case err := <-mintCanceled:
		t.Fatalf("the shared mint was canceled with the caller that started it: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseMint()

	for range callers {
		select {
		case got := <-results:
			if got.err != nil {
				t.Fatalf("waiter got %v, want the shared mint's result", got.err)
			}
			if got.token != "new" {
				t.Fatalf("waiter token = %q, want new", got.token)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a waiter did not return after the mint finished")
		}
	}
	if err := <-first; err != nil {
		t.Fatalf("the caller that started the mint: %v", err)
	}
}

func TestTokenSourceMintIsBoundedByTimeout(t *testing.T) {
	const mintTimeout = 50 * time.Millisecond
	source := &tokenSource{
		now:         time.Now,
		ttl:         time.Hour,
		background:  context.Background(),
		mintTimeout: mintTimeout,
		mint: func(ctx context.Context) (string, time.Time, error) {
			<-ctx.Done()
			return "", time.Time{}, ctx.Err()
		},
	}

	start := time.Now()
	_, err := source.get(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("get error = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("mint returned after %v, want it bounded by %v", elapsed, mintTimeout)
	}
}

func TestAteAPIClientRenewsTokenAfterInitContextCanceled(t *testing.T) {
	serverCert, caPEM := newTestServerCertificate(t)
	impl := &fakeControlServer{failFirst: 0}
	port, stopServer := startFakeControlServer(t, serverCert, impl)
	defer stopServer()

	const ttl = 120 * time.Millisecond
	clientset := fakeClientset(t, false, []testBundle{matchingBundle(caPEM)})
	recorder := addTokenReactor(clientset, ttl)
	var forwards atomic.Int64

	initCtx, cancelInit := context.WithCancel(context.Background())
	client, closeClient, err := newAteAPIClientWithDeps(
		initCtx,
		ateAPIConnConfig{ServiceAccount: "castworks-system/sandboxprovider"},
		testDeps(clientset, port, &forwards, ttl),
	)
	if err != nil {
		t.Fatalf("newAteAPIClientWithDeps: %v", err)
	}
	// The initialization context is no longer needed once the client exists.
	cancelInit()

	waitFor(t, 5*time.Second, func() bool { return recorder.count() >= 3 })

	if _, err := client.GetActor(context.Background(), &ateapi.GetActorRequest{}); err != nil {
		t.Fatalf("GetActor after init context cancellation: %v", err)
	}
	if got := impl.lastAuth(); got == "Bearer token-1" {
		t.Fatal("RPC carried the initial token after renewal")
	}

	beforeClose := recorder.count()
	if err := closeClient(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Close waits for the renewal goroutine, so no mint can start afterwards.
	if got := recorder.count(); got != beforeClose {
		t.Fatalf("token renewal continued after close: %d -> %d", beforeClose, got)
	}
}

func TestAteAPIClientConcurrentCalls(t *testing.T) {
	serverCert, caPEM := newTestServerCertificate(t)
	// The first call returns Unavailable, so one goroutine rebuilds the forward
	// while the others are calling through the old one.
	impl := &fakeControlServer{failFirst: 1}
	port, stopServer := startFakeControlServer(t, serverCert, impl)
	defer stopServer()

	clientset := fakeClientset(t, false, []testBundle{matchingBundle(caPEM)})
	addTokenReactor(clientset, time.Hour)
	var forwards atomic.Int64
	client, closeClient, err := newAteAPIClientWithDeps(
		context.Background(),
		ateAPIConnConfig{ServiceAccount: "castworks-system/sandboxprovider"},
		testDeps(clientset, port, &forwards, time.Hour),
	)
	if err != nil {
		t.Fatalf("newAteAPIClientWithDeps: %v", err)
	}
	defer func() { _ = closeClient() }()

	const goroutines, calls = 8, 20
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*calls)
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range calls {
				if _, err := client.GetActor(context.Background(), &ateapi.GetActorRequest{}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("GetActor: %v", err)
	}
}

func TestSplitServiceAccount(t *testing.T) {
	tests := []struct {
		in        string
		namespace string
		name      string
		wantErr   bool
	}{
		{in: "castworks-system/sandboxprovider", namespace: "castworks-system", name: "sandboxprovider"},
		{in: "", wantErr: true},
		{in: "sandboxprovider", wantErr: true},
		{in: "/sandboxprovider", wantErr: true},
		{in: "castworks-system/", wantErr: true},
		{in: "a/b/c", wantErr: true},
	}
	for _, tt := range tests {
		namespace, name, err := splitServiceAccount(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("splitServiceAccount(%q) succeeded, want error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitServiceAccount(%q): %v", tt.in, err)
			continue
		}
		if namespace != tt.namespace || name != tt.name {
			t.Errorf("splitServiceAccount(%q) = %q, %q, want %q, %q", tt.in, namespace, name, tt.namespace, tt.name)
		}
	}
}

// fakeControlServer answers GetActor and records the calls it sees.
type fakeControlServer struct {
	ateapi.UnimplementedControlServer

	mu        sync.Mutex
	calls     int
	auth      []string
	failFirst int    // calls up to this number return Unavailable; -1 returns it always
	gate      func() // called before responding, outside the lock
}

func (s *fakeControlServer) GetActor(ctx context.Context, _ *ateapi.GetActorRequest) (*ateapi.Actor, error) {
	s.mu.Lock()
	s.calls++
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		s.auth = append(s.auth, md.Get("authorization")...)
	}
	fail := s.failFirst < 0 || s.calls <= s.failFirst
	gate := s.gate
	s.mu.Unlock()

	if gate != nil {
		gate()
	}
	if fail {
		return nil, status.Error(codes.Unavailable, "temporarily unavailable")
	}
	return &ateapi.Actor{}, nil
}

func (s *fakeControlServer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *fakeControlServer) lastAuth() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.auth) == 0 {
		return ""
	}
	return s.auth[len(s.auth)-1]
}

func startFakeControlServer(t *testing.T, cert tls.Certificate, impl *fakeControlServer) (port int, stop func()) {
	t.Helper()
	return startControlServer(t, cert, impl)
}

func startControlServer(t *testing.T, cert tls.Certificate, impl ateapi.ControlServer) (port int, stop func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})))
	ateapi.RegisterControlServer(server, impl)
	go func() {
		_ = server.Serve(listener)
	}()
	return listener.Addr().(*net.TCPAddr).Port, server.Stop
}

func baseTestDeps(clientset kubernetes.Interface) ateAPIDeps {
	return ateAPIDeps{
		loadKubeConfig: func(string, string) (*rest.Config, error) { return &rest.Config{}, nil },
		newClientset:   func(*rest.Config) (kubernetes.Interface, error) { return clientset, nil },
		now:            time.Now,
		tokenTTL:       time.Hour,
	}
}

func testDeps(clientset kubernetes.Interface, port int, forwards *atomic.Int64, ttl time.Duration) ateAPIDeps {
	deps := baseTestDeps(clientset)
	deps.tokenTTL = ttl
	deps.startForward = func(context.Context, *rest.Config, kubernetes.Interface) (int, func(), error) {
		forwards.Add(1)
		return port, func() {}, nil
	}
	return deps
}

// recordedTokenRequest is one TokenRequest seen by the fake clientset.
type recordedTokenRequest struct {
	namespace         string
	name              string
	audiences         []string
	expirationSeconds int64
}

type tokenRequestRecorder struct {
	mu       sync.Mutex
	requests []recordedTokenRequest
	mints    atomic.Int64
}

func (r *tokenRequestRecorder) record(request recordedTokenRequest) int64 {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	r.mu.Unlock()
	return r.mints.Add(1)
}

func (r *tokenRequestRecorder) count() int64 { return r.mints.Load() }

func (r *tokenRequestRecorder) all() []recordedTokenRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedTokenRequest(nil), r.requests...)
}

func addTokenReactor(clientset *fake.Clientset, ttl time.Duration) *tokenRequestRecorder {
	recorder := &tokenRequestRecorder{}
	clientset.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		request, ok := create.GetObject().(*authv1.TokenRequest)
		if !ok {
			return false, nil, nil
		}
		var expirationSeconds int64
		if request.Spec.ExpirationSeconds != nil {
			expirationSeconds = *request.Spec.ExpirationSeconds
		}
		// CreateAction does not expose the resource name, but the concrete
		// action created for a subresource POST carries it.
		var name string
		switch a := action.(type) {
		case k8stesting.CreateActionImpl:
			name = a.Name
		case *k8stesting.CreateActionImpl:
			name = a.Name
		}
		n := recorder.record(recordedTokenRequest{
			namespace:         create.GetNamespace(),
			name:              name,
			audiences:         append([]string(nil), request.Spec.Audiences...),
			expirationSeconds: expirationSeconds,
		})
		response := request.DeepCopy()
		response.Status.Token = fmt.Sprintf("token-%d", n)
		response.Status.ExpirationTimestamp = metav1.NewTime(time.Now().Add(ttl))
		return true, response, nil
	})
	return recorder
}

type testBundle struct {
	name   string
	signer string
	labels map[string]string
	pem    []byte
}

func matchingBundle(caPEM []byte) testBundle {
	return testBundle{
		name:   "live",
		signer: clusterTrustBundleSigner,
		labels: map[string]string{"podcert.ate.dev/canarying": "live"},
		pem:    caPEM,
	}
}

func fakeClientset(t *testing.T, beta bool, bundles []testBundle) *fake.Clientset {
	t.Helper()
	clientset := fake.NewSimpleClientset()
	ctx := context.Background()

	if beta {
		clientset.Resources = []*metav1.APIResourceList{{
			GroupVersion: certsv1beta1.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{{Name: "clustertrustbundles", Kind: "ClusterTrustBundle"}},
		}}
		for _, bundle := range bundles {
			_, err := clientset.CertificatesV1beta1().ClusterTrustBundles().Create(ctx, &certsv1beta1.ClusterTrustBundle{
				ObjectMeta: metav1.ObjectMeta{Name: bundle.name, Labels: bundle.labels},
				Spec: certsv1beta1.ClusterTrustBundleSpec{
					SignerName:  bundle.signer,
					TrustBundle: string(bundle.pem),
				},
			}, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("create v1beta1 ClusterTrustBundle %q: %v", bundle.name, err)
			}
		}
		return clientset
	}

	clientset.Resources = []*metav1.APIResourceList{{
		GroupVersion: certsv1.SchemeGroupVersion.String(),
		APIResources: []metav1.APIResource{{Name: "clustertrustbundles", Kind: "ClusterTrustBundle"}},
	}}
	for _, bundle := range bundles {
		_, err := clientset.CertificatesV1().ClusterTrustBundles().Create(ctx, &certsv1.ClusterTrustBundle{
			ObjectMeta: metav1.ObjectMeta{Name: bundle.name, Labels: bundle.labels},
			Spec: certsv1.ClusterTrustBundleSpec{
				SignerName:  bundle.signer,
				TrustBundle: string(bundle.pem),
			},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create ClusterTrustBundle %q: %v", bundle.name, err)
		}
	}
	return clientset
}

// newTestServerCertificate returns a self-signed serving certificate for
// api.ate-system.svc and its PEM encoding, which doubles as the trust root.
func newTestServerCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: ateServerName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{ateServerName},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

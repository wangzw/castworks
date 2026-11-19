// The port-forward, ClusterTrustBundle and bearer-token code in this file is
// adapted from Substrate v1.0.0's internal/portforward/portforward.go,
// internal/clustertrustbundle/client.go and internal/ateclient/builder.go and
// is used under the Apache License, Version 2.0.
//
// It differs from builder.go in two ways the provider needs: the bearer token
// is re-minted halfway through its lifetime, and an RPC that returns
// Unavailable rebuilds the port-forward and is retried once.

package substrate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	authv1 "k8s.io/api/authentication/v1"
	certsv1 "k8s.io/api/certificates/v1"
	certsv1beta1 "k8s.io/api/certificates/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
)

// The ate-api location and identity are fixed for v0.1.0; they are not
// configurable through Config.
const (
	ateSystemNamespace = "ate-system"
	ateAPIServiceName  = "api"
	ateAPIServicePort  = int32(443)
	ateServerName      = "api.ate-system.svc"
	ateTokenAudience   = "api.ate-system.svc"
	ateTokenTTL        = time.Hour

	clusterTrustBundleSigner = "servicedns.podcert.ate.dev/identity"
	clusterTrustBundleLabel  = "podcert.ate.dev/canarying=live"
)

// ateAPIConnConfig carries the connection-related fields of Config.
type ateAPIConnConfig struct {
	Kubeconfig     string // 空则用默认加载规则
	KubeContext    string // 空则用当前 context
	ServiceAccount string // <namespace>/<name>
}

// defaultForwardRebuildTimeout bounds one port-forward rebuild. A rebuild that
// takes longer is abandoned so that a caller's RPC cannot wait forever.
const defaultForwardRebuildTimeout = 30 * time.Second

// defaultTokenMintTimeout bounds one bearer-token mint. After construction the
// mint runs on the connection's background context, so one caller's
// cancellation cannot abort a mint other callers are waiting for; this timeout
// is what ends a mint whose TokenRequest hangs.
const defaultTokenMintTimeout = 10 * time.Second

// ateAPIDeps holds the seams the connection uses. Production code uses
// defaultAteAPIDeps; tests replace the kubeconfig loading, the clientset, the
// port-forward, the token mint and the clock.
type ateAPIDeps struct {
	loadKubeConfig func(kubeconfig, kubeContext string) (*rest.Config, error)
	newClientset   func(*rest.Config) (kubernetes.Interface, error)
	startForward   func(ctx context.Context, cfg *rest.Config, cs kubernetes.Interface) (port int, stop func(), err error)
	mintToken      func(ctx context.Context, cs kubernetes.Interface, namespace, name, audience string, ttl time.Duration) (string, time.Time, error)
	now            func() time.Time
	tokenTTL       time.Duration
	rebuildTimeout time.Duration
}

var defaultAteAPIDeps = ateAPIDeps{
	loadKubeConfig: loadKubeConfig,
	newClientset: func(cfg *rest.Config) (kubernetes.Interface, error) {
		return kubernetes.NewForConfig(cfg)
	},
	startForward: func(ctx context.Context, cfg *rest.Config, cs kubernetes.Interface) (int, func(), error) {
		return servicePortForward(ctx, cfg, cs, ateSystemNamespace, ateAPIServiceName, ateAPIServicePort)
	},
	mintToken:      mintServiceAccountToken,
	now:            time.Now,
	tokenTTL:       ateTokenTTL,
	rebuildTimeout: defaultForwardRebuildTimeout,
}

// newAteAPIClient connects to ate-api and returns a ControlClient whose RPCs
// carry the latest bearer token. ctx bounds the initial port-forward, TLS setup
// and first token mint only. Later forward rebuilds and token renewals run on
// the connection's own context until the returned close function is called,
// and every mint is bounded by defaultTokenMintTimeout so that canceling the
// RPC that happened to trigger a refresh cannot abort a mint other callers are
// waiting for. The close function is idempotent.
func newAteAPIClient(ctx context.Context, cfg ateAPIConnConfig) (ateapi.ControlClient, func() error, error) {
	return newAteAPIClientWithDeps(ctx, cfg, defaultAteAPIDeps)
}

func newAteAPIClientWithDeps(ctx context.Context, cfg ateAPIConnConfig, deps ateAPIDeps) (ateapi.ControlClient, func() error, error) {
	saNamespace, saName, err := splitServiceAccount(cfg.ServiceAccount)
	if err != nil {
		return nil, nil, err
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	if deps.tokenTTL <= 0 {
		deps.tokenTTL = ateTokenTTL
	}
	if deps.mintToken == nil {
		deps.mintToken = mintServiceAccountToken
	}
	if deps.rebuildTimeout <= 0 {
		deps.rebuildTimeout = defaultForwardRebuildTimeout
	}

	restConfig, err := deps.loadKubeConfig(cfg.Kubeconfig, cfg.KubeContext)
	if err != nil {
		return nil, nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	// Older clusters serve only the beta ClusterTrustBundle API; suppress the
	// deprecation warning when falling back to it.
	restConfig.WarningHandlerWithContext = &rest.NoWarnings{}

	clientset, err := deps.newClientset(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("create Kubernetes client: %w", err)
	}

	port, stopForward, err := deps.startForward(ctx, restConfig, clientset)
	if err != nil {
		return nil, nil, fmt.Errorf("forward to ate-api: %w", err)
	}

	tlsConfig, err := serverTLSConfig(ctx, clientset, ateServerName)
	if err != nil {
		stopForward()
		return nil, nil, err
	}

	tokens := &tokenSource{
		mint: func(ctx context.Context) (string, time.Time, error) {
			return deps.mintToken(ctx, clientset, saNamespace, saName, ateTokenAudience, deps.tokenTTL)
		},
		now: deps.now,
		ttl: deps.tokenTTL,
	}
	// The first token is minted under the initialization context.
	if err := tokens.refresh(ctx); err != nil {
		stopForward()
		return nil, nil, err
	}

	background, cancelBackground := context.WithCancel(context.Background())
	tokens.background = background
	tokens.mintTimeout = defaultTokenMintTimeout
	c := &ateAPIClient{
		clientset:  clientset,
		restConfig: restConfig,
		deps:       deps,
		tlsConfig:  tlsConfig,
		tokens:     tokens,
		background: background,
	}

	conn, err := c.dial(fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		cancelBackground()
		stopForward()
		return nil, nil, err
	}
	c.forward = &ateAPIForward{gen: 1, conn: conn, stop: stopForward}
	c.forwardGen = 1

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		tokens.run(background)
	}()

	var closeOnce sync.Once
	var closeErr error
	closeClient := func() error {
		closeOnce.Do(func() {
			cancelBackground()
			// Wait for the token renewal goroutine to observe the canceled
			// context, so no renewal can mint after Close returns.
			c.wg.Wait()

			c.mu.Lock()
			c.closed = true
			forwards := make([]*ateAPIForward, 0, 1+len(c.retired))
			if c.forward != nil {
				forwards = append(forwards, c.forward)
			}
			for f := range c.retired {
				forwards = append(forwards, f)
			}
			c.retired = nil
			c.mu.Unlock()

			var errs []error
			for _, f := range forwards {
				if err := f.close(); err != nil {
					errs = append(errs, err)
				}
			}
			closeErr = errors.Join(errs...)
		})
		return closeErr
	}

	return ateapi.NewControlClient(c), closeClient, nil
}

// ateAPIForward is one port-forward generation and the RPCs currently using
// it. inflight and retired are guarded by ateAPIClient.mu.
type ateAPIForward struct {
	gen      uint64
	conn     *grpc.ClientConn
	stop     func()
	inflight int
	retired  bool

	closeOnce sync.Once
	closeErr  error
}

// close stops the forward and closes its client connection. The caller must
// ensure no RPC is in flight on it. Close and the last in-flight release may
// race, so closing is idempotent.
func (f *ateAPIForward) close() error {
	f.closeOnce.Do(func() {
		if f.stop != nil {
			f.stop()
			f.stop = nil
		}
		if f.conn != nil {
			f.closeErr = f.conn.Close()
			f.conn = nil
		}
	})
	return f.closeErr
}

// ateAPIClient is a grpc.ClientConnInterface that retries an RPC once after
// rebuilding the port-forward when it returns Unavailable.
type ateAPIClient struct {
	clientset  kubernetes.Interface
	restConfig *rest.Config
	deps       ateAPIDeps
	tlsConfig  *tls.Config
	tokens     *tokenSource
	background context.Context

	mu         sync.Mutex
	forward    *ateAPIForward
	forwardGen uint64
	rebuilding bool
	// rebuildWait is closed when the rebuild in progress finishes.
	rebuildWait chan struct{}
	closed      bool
	// retired holds forwards replaced by a rebuild while an RPC was still
	// using them. The last one out stops and closes its forward.
	retired map[*ateAPIForward]struct{}
	wg      sync.WaitGroup
}

var _ grpc.ClientConnInterface = (*ateAPIClient)(nil)

func (c *ateAPIClient) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	gen, err := c.invoke(ctx, method, args, reply, opts...)
	if status.Code(err) != codes.Unavailable {
		return err
	}
	if rebuildErr := c.rebuildForward(ctx, gen); rebuildErr != nil {
		slog.Default().WarnContext(ctx, "ate-api port-forward rebuild failed", "error", rebuildErr)
		return err
	}
	_, retryErr := c.invoke(ctx, method, args, reply, opts...)
	return retryErr
}

// invoke makes one call on the current forward and reports the generation it
// used.
func (c *ateAPIClient) invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) (uint64, error) {
	forward, conn, err := c.acquire()
	if err != nil {
		return 0, err
	}
	err = conn.Invoke(ctx, method, args, reply, opts...)
	c.release(forward)
	return forward.gen, err
}

// NewStream dials the current forward. The Control service has only unary
// RPCs, so streams are not counted as in-flight calls.
func (c *ateAPIClient) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("ate-api connection is closed")
	}
	conn := c.forward.conn
	c.mu.Unlock()
	return conn.NewStream(ctx, desc, method, opts...)
}

// acquire marks one in-flight call on the current forward and returns its
// connection. The connection pointer is copied under the lock so that Close
// clearing it cannot race with the call.
func (c *ateAPIClient) acquire() (*ateAPIForward, *grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, nil, errors.New("ate-api connection is closed")
	}
	c.forward.inflight++
	return c.forward, c.forward.conn, nil
}

// release ends an in-flight call. A retired forward with no calls left is
// stopped immediately instead of lingering until Close.
func (c *ateAPIClient) release(forward *ateAPIForward) {
	c.mu.Lock()
	forward.inflight--
	closeNow := false
	if forward.inflight == 0 && forward.retired {
		delete(c.retired, forward)
		closeNow = true
	}
	c.mu.Unlock()
	if closeNow {
		_ = forward.close()
	}
}

// rebuildForward replaces the port-forward generation gen used by a failed
// RPC. The network work runs outside the lock, only one caller rebuilds a
// given generation, and a caller waiting for that rebuild selects its own
// context.
func (c *ateAPIClient) rebuildForward(ctx context.Context, gen uint64) error {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return errors.New("ate-api connection is closed")
		}
		if c.forwardGen != gen {
			// Another goroutine already replaced this generation.
			c.mu.Unlock()
			return nil
		}
		if c.rebuilding {
			wait := c.rebuildWait
			c.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		c.rebuilding = true
		c.rebuildWait = make(chan struct{})
		wait := c.rebuildWait
		c.mu.Unlock()

		next, err := c.startForward(ctx)

		var retire []*ateAPIForward
		c.mu.Lock()
		if err == nil {
			if c.closed {
				err = errors.New("ate-api connection is closed")
			} else {
				next.gen = c.forwardGen + 1
				c.forwardGen = next.gen
				old := c.forward
				c.forward = next
				if old != nil {
					old.retired = true
					if old.inflight == 0 {
						retire = append(retire, old)
					} else {
						if c.retired == nil {
							c.retired = make(map[*ateAPIForward]struct{})
						}
						c.retired[old] = struct{}{}
					}
				}
			}
		}
		c.rebuilding = false
		close(wait)
		c.mu.Unlock()

		if err != nil && next != nil {
			_ = next.close()
		}
		for _, forward := range retire {
			_ = forward.close()
		}
		return err
	}
}

// startForward establishes one new port-forward and dials it. The rebuild is
// bounded by deps.rebuildTimeout and is abandoned when the connection closes.
func (c *ateAPIClient) startForward(ctx context.Context) (*ateAPIForward, error) {
	buildCtx, cancel := context.WithTimeout(ctx, c.deps.rebuildTimeout)
	stopOnClose := context.AfterFunc(c.background, cancel)
	defer func() {
		stopOnClose()
		cancel()
	}()

	port, stop, err := c.deps.startForward(buildCtx, c.restConfig, c.clientset)
	if err != nil {
		return nil, fmt.Errorf("forward to ate-api: %w", err)
	}
	conn, err := c.dial(fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		stop()
		return nil, err
	}
	return &ateAPIForward{conn: conn, stop: stop}, nil
}

func (c *ateAPIClient) dial(endpoint string) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(
		"passthrough:///"+endpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(c.tlsConfig)),
		grpc.WithPerRPCCredentials(tokenCredentials{tokens: c.tokens}),
	)
	if err != nil {
		return nil, fmt.Errorf("dial ate-api: %w", err)
	}
	return conn, nil
}

// tokenCredentials presents the current bearer token to every RPC.
type tokenCredentials struct {
	tokens *tokenSource
}

func (c tokenCredentials) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	token, err := c.tokens.get(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + token}, nil
}

func (tokenCredentials) RequireTransportSecurity() bool { return true }

// tokenSource keeps one bearer token and re-mints it halfway through its
// lifetime. A failed renewal keeps the old token while it is still valid.
// Minting happens outside the lock, so an RPC holding a still-valid token is
// never blocked by a slow TokenRequest. The mint itself runs on background,
// not on the context of the RPC that triggered it; waiters still select their
// own context while they wait.
type tokenSource struct {
	mu        sync.Mutex
	token     string
	renewAt   time.Time
	expiresAt time.Time

	// refreshing is set while one mint is in progress; refreshDone is closed
	// when it finishes and refreshErr holds its result.
	refreshing  bool
	refreshDone chan struct{}
	refreshErr  error

	mint func(ctx context.Context) (token string, expiresAt time.Time, err error)
	now  func() time.Time
	ttl  time.Duration

	// background bounds mints after construction. It is nil for the first mint,
	// which runs under the caller's initialization context.
	background  context.Context
	mintTimeout time.Duration

	// onWait, when non-nil, is called by a caller that is about to block on a
	// refresh in progress. Tests use it to observe that a waiter has joined
	// the shared refresh before they cancel the caller that started it.
	onWait func()
}

// get returns the current token, minting a replacement when the token is past
// its renewal point. If a mint is already in progress and the old token is
// still valid, the old token is returned without waiting. If minting fails
// while the old token is still valid, the old token is returned.
func (s *tokenSource) get(ctx context.Context) (string, error) {
	for {
		s.mu.Lock()
		if s.token != "" && s.now().Before(s.renewAt) {
			token := s.token
			s.mu.Unlock()
			return token, nil
		}
		if s.refreshing {
			// A renewal is due but another goroutine is already minting it.
			// Hand out the still-valid token instead of blocking the RPC.
			if s.token != "" && s.now().Before(s.expiresAt) {
				token := s.token
				s.mu.Unlock()
				return token, nil
			}
			wait := s.refreshDone
			s.mu.Unlock()
			if s.onWait != nil {
				s.onWait()
			}
			select {
			case <-wait:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			continue
		}
		s.mu.Unlock()

		if err := s.renew(ctx); err != nil {
			s.mu.Lock()
			now := s.now()
			if s.token != "" && now.Before(s.expiresAt) {
				// Keep the old token and wait before minting again, so that a
				// failing TokenRequest does not make every RPC retry it.
				retry := 30 * time.Second
				if half := s.expiresAt.Sub(now) / 2; half < retry {
					retry = half
				}
				if retry <= 0 {
					retry = time.Second
				}
				s.renewAt = now.Add(retry)
				token := s.token
				s.mu.Unlock()
				return token, nil
			}
			s.mu.Unlock()
			return "", err
		}
		s.mu.Lock()
		token := s.token
		s.mu.Unlock()
		return token, nil
	}
}

// refresh mints a new token unconditionally.
func (s *tokenSource) refresh(ctx context.Context) error {
	return s.renew(ctx)
}

// renew mints a new token. Concurrent callers share one mint and receive its
// result; the mint itself runs without the lock held, on the connection's
// background context after construction, and waiters select their own context.
func (s *tokenSource) renew(ctx context.Context) error {
	s.mu.Lock()
	if s.refreshing {
		wait := s.refreshDone
		s.mu.Unlock()
		if s.onWait != nil {
			s.onWait()
		}
		select {
		case <-wait:
			s.mu.Lock()
			err := s.refreshErr
			s.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.refreshing = true
	s.refreshDone = make(chan struct{})
	wait := s.refreshDone
	s.mu.Unlock()

	mintCtx, cancel := s.mintContext(ctx)
	defer cancel()
	token, expiresAt, err := s.mint(mintCtx)

	s.mu.Lock()
	if err == nil {
		if token == "" {
			err = errors.New("mint ate-api bearer token: empty token")
		} else {
			now := s.now()
			if expiresAt.IsZero() {
				expiresAt = now.Add(s.ttl)
			}
			lifetime := expiresAt.Sub(now)
			if lifetime <= 0 {
				err = errors.New("mint ate-api bearer token: already expired")
			} else {
				s.token = token
				s.expiresAt = expiresAt
				s.renewAt = now.Add(lifetime / 2)
			}
		}
	}
	s.refreshErr = err
	s.refreshing = false
	close(wait)
	s.mu.Unlock()
	return err
}

// mintContext returns the context one mint runs under. After construction that
// is the connection's background context, so canceling the RPC that happened
// to trigger the refresh cannot abort a mint other callers are waiting for.
// The first mint, before the client exists, is bounded by the initialization
// context instead. Either way the mint has its own timeout.
func (s *tokenSource) mintContext(caller context.Context) (context.Context, context.CancelFunc) {
	base := s.background
	if base == nil {
		base = caller
	}
	timeout := s.mintTimeout
	if timeout <= 0 {
		timeout = defaultTokenMintTimeout
	}
	return context.WithTimeout(base, timeout)
}

// run renews the token until ctx is canceled. It never logs the token.
func (s *tokenSource) run(ctx context.Context) {
	const retryDelay = 30 * time.Second
	for {
		s.mu.Lock()
		wait := s.renewAt.Sub(s.now())
		s.mu.Unlock()
		if !sleepContext(ctx, wait) {
			return
		}
		if err := s.refresh(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Default().WarnContext(ctx, "ate-api bearer token renewal failed", "error", err)
			if !sleepContext(ctx, retryDelay) {
				return
			}
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func splitServiceAccount(s string) (namespace, name string, err error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid ServiceAccount %q: want <namespace>/<name>", s)
	}
	return parts[0], parts[1], nil
}

// mintServiceAccountToken requests a token for one ServiceAccount and returns
// it with its expiry. The token itself is never included in errors or logs.
func mintServiceAccountToken(ctx context.Context, clientset kubernetes.Interface, namespace, name, audience string, ttl time.Duration) (string, time.Time, error) {
	expirationSeconds := int64(ttl / time.Second)
	request := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: &expirationSeconds,
		},
	}
	response, err := clientset.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, name, request, metav1.CreateOptions{})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("request ate-api bearer token: %w", err)
	}
	if response.Status.Token == "" {
		return "", time.Time{}, errors.New("request ate-api bearer token: empty token")
	}
	return response.Status.Token, response.Status.ExpirationTimestamp.Time, nil
}

// loadKubeConfig loads a Kubernetes client configuration from the specified
// kubeconfig path and context.
func loadKubeConfig(kubeconfigPath, kubeContext string) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = kubeconfigPath
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
}

// serverTLSConfig verifies a Service's servicedns serving certificate for
// serverName using the live ClusterTrustBundle.
func serverTLSConfig(ctx context.Context, clientset kubernetes.Interface, serverName string) (*tls.Config, error) {
	gv, err := discoverClusterTrustBundle(clientset.Discovery())
	if err != nil {
		return nil, fmt.Errorf("discover ClusterTrustBundle API: %w", err)
	}
	bundles, err := listClusterTrustBundles(ctx, clientset, gv, metav1.ListOptions{LabelSelector: clusterTrustBundleLabel})
	if err != nil {
		return nil, fmt.Errorf("list ClusterTrustBundles: %w", err)
	}

	selector, err := labels.Parse(clusterTrustBundleLabel)
	if err != nil {
		return nil, fmt.Errorf("parse ClusterTrustBundle label selector: %w", err)
	}
	pool := x509.NewCertPool()
	found := false
	for i := range bundles.Items {
		bundle := &bundles.Items[i]
		if bundle.Spec.SignerName != clusterTrustBundleSigner {
			continue
		}
		// The API server filters by label; checking it here as well keeps the
		// rule explicit and testable against a fake clientset.
		if !selector.Matches(labels.Set(bundle.Labels)) {
			continue
		}
		if !pool.AppendCertsFromPEM([]byte(bundle.Spec.TrustBundle)) {
			return nil, fmt.Errorf("ClusterTrustBundle %q contains no valid certificates", bundle.Name)
		}
		found = true
	}
	if !found {
		return nil, fmt.Errorf("no live ClusterTrustBundle found for signer %q", clusterTrustBundleSigner)
	}

	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
		ServerName: serverName,
	}, nil
}

// discoverClusterTrustBundle prefers certificates.k8s.io/v1 and falls back to
// v1beta1 only when discovery reports the stable API absent.
func discoverClusterTrustBundle(d discovery.DiscoveryInterface) (schema.GroupVersion, error) {
	for _, gv := range []schema.GroupVersion{certsv1.SchemeGroupVersion, certsv1beta1.SchemeGroupVersion} {
		resources, err := d.ServerResourcesForGroupVersion(gv.String())
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return schema.GroupVersion{}, fmt.Errorf("discover ClusterTrustBundle %s: %w", gv.Version, err)
		}
		for _, resource := range resources.APIResources {
			if resource.Name == "clustertrustbundles" {
				return gv, nil
			}
		}
	}
	return schema.GroupVersion{}, errors.New("neither v1 nor v1beta1 ClusterTrustBundle is served")
}

func listClusterTrustBundles(ctx context.Context, clientset kubernetes.Interface, gv schema.GroupVersion, opts metav1.ListOptions) (*certsv1.ClusterTrustBundleList, error) {
	if gv == certsv1.SchemeGroupVersion {
		return clientset.CertificatesV1().ClusterTrustBundles().List(ctx, opts)
	}
	list, err := clientset.CertificatesV1beta1().ClusterTrustBundles().List(ctx, opts)
	if err != nil {
		return nil, err
	}
	converted := &certsv1.ClusterTrustBundleList{
		ListMeta: list.ListMeta,
		Items:    make([]certsv1.ClusterTrustBundle, len(list.Items)),
	}
	for i := range list.Items {
		converted.Items[i] = *toV1ClusterTrustBundle(&list.Items[i])
	}
	return converted, nil
}

func toV1ClusterTrustBundle(bundle *certsv1beta1.ClusterTrustBundle) *certsv1.ClusterTrustBundle {
	if bundle == nil {
		return nil
	}
	return &certsv1.ClusterTrustBundle{
		TypeMeta:   metav1.TypeMeta{APIVersion: certsv1.SchemeGroupVersion.String(), Kind: "ClusterTrustBundle"},
		ObjectMeta: *bundle.ObjectMeta.DeepCopy(),
		Spec:       certsv1.ClusterTrustBundleSpec(bundle.Spec),
	}
}

// servicePortForward forwards a random local port to servicePort of the named
// Service. It picks a ready backing pod, resolves servicePort to the pod's
// container port, and returns the local port together with a stop function.
func servicePortForward(ctx context.Context, config *rest.Config, clientset kubernetes.Interface, namespace, service string, servicePort int32) (int, func(), error) {
	pod, svc, err := firstReadyPodForService(ctx, clientset, namespace, service)
	if err != nil {
		return 0, nil, err
	}
	targetPort, err := resolveTargetPort(svc, pod, servicePort)
	if err != nil {
		return 0, nil, err
	}
	return podPortForward(ctx, config, clientset, namespace, pod.Name, targetPort)
}

func firstReadyPodForService(ctx context.Context, clientset kubernetes.Interface, namespace, service string) (*corev1.Pod, *corev1.Service, error) {
	svc, err := clientset.CoreV1().Services(namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("get %s Service: %w", service, err)
	}
	if len(svc.Spec.Selector) == 0 {
		return nil, nil, fmt.Errorf("service %s has no selector", service)
	}
	selector := labels.SelectorFromSet(svc.Spec.Selector).String()
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, nil, fmt.Errorf("list %s pods: %w", service, err)
	}
	for i := range pods.Items {
		if isPodReady(&pods.Items[i]) {
			return &pods.Items[i], svc, nil
		}
	}
	return nil, nil, fmt.Errorf("no ready %s pods in %s", service, namespace)
}

func resolveTargetPort(svc *corev1.Service, pod *corev1.Pod, servicePort int32) (int32, error) {
	for _, sp := range svc.Spec.Ports {
		if sp.Port != servicePort {
			continue
		}
		var port int32
		switch sp.TargetPort.Type {
		case intstr.Int:
			port = sp.TargetPort.IntVal
			if port == 0 {
				// targetPort defaults to port when omitted.
				port = sp.Port
			}
		case intstr.String:
			for _, c := range pod.Spec.Containers {
				for _, cp := range c.Ports {
					if cp.Name == sp.TargetPort.StrVal {
						port = cp.ContainerPort
					}
				}
			}
			if port == 0 {
				return 0, fmt.Errorf("named targetPort %q not found on pod %s", sp.TargetPort.StrVal, pod.Name)
			}
		}
		return port, nil
	}
	return 0, fmt.Errorf("service %s has no port %d", svc.Name, servicePort)
}

func podPortForward(ctx context.Context, config *rest.Config, clientset kubernetes.Interface, namespace, podName string, targetPort int32) (int, func(), error) {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward")

	dialer, err := portforward.NewSPDYOverWebsocketDialer(req.URL(), config)
	if err != nil {
		return 0, nil, fmt.Errorf("create websocket dialer: %w", err)
	}

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", targetPort)}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return 0, nil, fmt.Errorf("create port forwarder: %w", err)
	}

	errCh := make(chan error, 1)
	go func() {
		if err := fw.ForwardPorts(); err != nil {
			errCh <- err
		}
	}()
	select {
	case <-readyCh:
	case err := <-errCh:
		return 0, nil, fmt.Errorf("port forwarding: %w", err)
	case <-ctx.Done():
		close(stopCh)
		return 0, nil, ctx.Err()
	}

	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		close(stopCh)
		return 0, nil, fmt.Errorf("get forwarded ports: %w", err)
	}
	return int(ports[0].Local), func() { close(stopCh) }, nil
}

func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

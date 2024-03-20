package healthcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	gokitmetrics "github.com/go-kit/kit/metrics"
	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/config/dynamic"
	"github.com/traefik/traefik/v3/pkg/config/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

const modeGRPC = "grpc"

var (
	singleton *HealthCheck
	once      sync.Once
)

// Balancer is the set of operations required to manage the list of servers in a load-balancer.
type Balancer interface {
	Servers() []*url.URL
	RemoveServer(u *url.URL) error
	UpsertServer(u *url.URL, options ...roundrobin.ServerOption) error
}

// BalancerHandler includes functionality for load-balancing management.
type BalancerHandler interface {
	ServeHTTP(w http.ResponseWriter, req *http.Request)
	Balancer
}

// BalancerStatusHandler is an http Handler that does load-balancing,
// and updates its parents of its status.
type BalancerStatusHandler interface {
	BalancerHandler
	StatusUpdater
}

type metricsHealthcheck struct {
	serverUpGauge gokitmetrics.Gauge
}

// Options are the public health check options.
type Options struct {
	Headers         map[string]string
	Hostname        string
	Scheme          string
	Path            string
	Method          string
	Port            int
	FollowRedirects bool
	Transport       http.RoundTripper
	Interval        time.Duration
	Timeout         time.Duration
	LB              Balancer
}

func (opt Options) String() string {
	return fmt.Sprintf("[Hostname: %s Headers: %v Path: %s Method: %s Port: %d Interval: %s Timeout: %s FollowRedirects: %v]", opt.Hostname, opt.Headers, opt.Path, opt.Method, opt.Port, opt.Interval, opt.Timeout, opt.FollowRedirects)
}

type backendURL struct {
	url    *url.URL
	weight int
}

// BackendConfig HealthCheck configuration for a backend.
type BackendConfig struct {
	Options
	name         string
	disabledURLs []backendURL
}

func (b *BackendConfig) newRequest(serverURL *url.URL) (*http.Request, error) {
	u, err := serverURL.Parse(b.Path)
	if err != nil {
		return nil, err
	}

	if len(b.Scheme) > 0 {
		u.Scheme = b.Scheme
	}

	if b.Port != 0 {
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(b.Port))
	}

	return http.NewRequest(http.MethodGet, u.String(), http.NoBody)
}

// setRequestOptions sets all request options present on the BackendConfig.
func (b *BackendConfig) setRequestOptions(req *http.Request) *http.Request {
	if b.Options.Hostname != "" {
		req.Host = b.Options.Hostname
	}

	for k, v := range b.Options.Headers {
		req.Header.Set(k, v)
	}

	if b.Options.Method != "" {
		req.Method = strings.ToUpper(b.Options.Method)
	}

	return req
}

// HealthCheck struct.
type HealthCheck struct {
	Backends map[string]*BackendConfig
	metrics  metricsHealthcheck
	cancel   context.CancelFunc
}

// SetBackendsConfiguration set backends configuration.
func (hc *HealthCheck) SetBackendsConfiguration(parentCtx context.Context, backends map[string]*BackendConfig) {
	hc.Backends = backends
	if hc.cancel != nil {
		hc.cancel()
	}
	ctx, cancel := context.WithCancel(parentCtx)
	hc.cancel = cancel

	for _, backend := range backends {
		safe.Go(func() {
			hc.execute(ctx, backend)
		})
	}
}

func (hc *HealthCheck) execute(ctx context.Context, backend *BackendConfig) {
	logger := log.FromContext(ctx)

	logger.Debugf("Initial health check for backend: %q", backend.name)
	hc.checkServersLB(ctx, backend)

	ticker := time.NewTicker(backend.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Debugf("Stopping current health check goroutines of backend: %s", backend.name)
			return
		case <-ticker.C:
			logger.Debugf("Routine health check refresh for backend: %s", backend.name)
			hc.checkServersLB(ctx, backend)
		}
	}
}

func (hc *HealthCheck) checkServersLB(ctx context.Context, backend *BackendConfig) {
	logger := log.FromContext(ctx)

	enabledURLs := backend.LB.Servers()

	var newDisabledURLs []backendURL
	for _, disabledURL := range backend.disabledURLs {
		serverUpMetricValue := float64(0)

		if err := checkHealth(disabledURL.url, backend); err == nil {
			logger.Warnf("Health check up: returning to server list. Backend: %q URL: %q Weight: %d",
				backend.name, disabledURL.url.String(), disabledURL.weight)
			if err = backend.LB.UpsertServer(disabledURL.url, roundrobin.Weight(disabledURL.weight)); err != nil {
				logger.Error(err)
			}
			serverUpMetricValue = 1
		} else {
			logger.Warnf("Health check still failing. Backend: %q URL: %q Reason: %s", backend.name, disabledURL.url.String(), err)
			newDisabledURLs = append(newDisabledURLs, disabledURL)
		}

		labelValues := []string{"service", backend.name, "url", disabledURL.url.String()}
		hc.metrics.serverUpGauge.With(labelValues...).Set(serverUpMetricValue)
	}

	backend.disabledURLs = newDisabledURLs

	for _, enabledURL := range enabledURLs {
		serverUpMetricValue := float64(1)

		if err := checkHealth(enabledURL, backend); err != nil {
			weight := 1
			rr, ok := backend.LB.(*roundrobin.RoundRobin)
			if ok {
				var gotWeight bool
				weight, gotWeight = rr.ServerWeight(enabledURL)
				if !gotWeight {
					weight = 1
				}
			}

			logger.Warnf("Health check failed, removing from server list. Backend: %q URL: %q Weight: %d Reason: %s",
				backend.name, enabledURL.String(), weight, err)
			if err := backend.LB.RemoveServer(enabledURL); err != nil {
				logger.Error(err)
			}

			backend.disabledURLs = append(backend.disabledURLs, backendURL{enabledURL, weight})
			serverUpMetricValue = 0
		}

		labelValues := []string{"service", backend.name, "url", enabledURL.String()}
		hc.metrics.serverUpGauge.With(labelValues...).Set(serverUpMetricValue)
	}
}

// GetHealthCheck returns the health check which is guaranteed to be a singleton.
func GetHealthCheck(registry metrics.Registry) *HealthCheck {
	once.Do(func() {
		singleton = newHealthCheck(registry)
	})
	return singleton
}

func newHealthCheck(registry metrics.Registry) *HealthCheck {
	return &HealthCheck{
		Backends: make(map[string]*BackendConfig),
		metrics: metricsHealthcheck{
			serverUpGauge: registry.ServiceServerUpGauge(),
		},
	}
}

// NewBackendConfig Instantiate a new BackendConfig.
func NewBackendConfig(options Options, backendName string) *BackendConfig {
	return &BackendConfig{
		Options: options,
		name:    backendName,
	}
}

// checkHealth returns a nil error in case it was successful and otherwise
// a non-nil error with a meaningful description why the health check failed.
func checkHealth(serverURL *url.URL, backend *BackendConfig) error {
	req, err := backend.newRequest(serverURL)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req = backend.setRequestOptions(req)

	client := http.Client{
		Timeout:   backend.Options.Timeout,
		Transport: backend.Options.Transport,
	}

	if !backend.FollowRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("received error status code: %v", resp.StatusCode)
	}

	return nil
}

// StatusUpdater should be implemented by a service that, when its status
// changes (e.g. all if its children are down), needs to propagate upwards (to
// their parent(s)) that change.
type StatusUpdater interface {
	RegisterStatusUpdater(fn func(up bool)) error
}

type metricsHealthCheck interface {
	ServiceServerUpGauge() gokitmetrics.Gauge
}

type ServiceHealthChecker struct {
	balancer StatusSetter
	info     *runtime.ServiceInfo

	config   *dynamic.ServerHealthCheck
	interval time.Duration
	timeout  time.Duration

	metrics metricsHealthCheck

	client  *http.Client
	targets map[string]*url.URL
}

func NewServiceHealthChecker(ctx context.Context, metrics metricsHealthCheck, config *dynamic.ServerHealthCheck, service StatusSetter, info *runtime.ServiceInfo, transport http.RoundTripper, targets map[string]*url.URL) *ServiceHealthChecker {
	logger := log.Ctx(ctx)

	interval := time.Duration(config.Interval)
	if interval <= 0 {
		logger.Error().Msg("Health check interval smaller than zero")
		interval = time.Duration(dynamic.DefaultHealthCheckInterval)
	}

	timeout := time.Duration(config.Timeout)
	if timeout <= 0 {
		logger.Error().Msg("Health check timeout smaller than zero")
		timeout = time.Duration(dynamic.DefaultHealthCheckTimeout)
	}

	client := &http.Client{
		Transport: transport,
	}

	if config.FollowRedirects != nil && !*config.FollowRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	return &ServiceHealthChecker{
		balancer: service,
		info:     info,
		config:   config,
		interval: interval,
		timeout:  timeout,
		targets:  targets,
		client:   client,
		metrics:  metrics,
	}
}

func (shc *ServiceHealthChecker) Launch(ctx context.Context) {
	ticker := time.NewTicker(shc.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			for proxyName, target := range shc.targets {
				select {
				case <-ctx.Done():
					return
				default:
				}

				up := true
				serverUpMetricValue := float64(1)

				if err := shc.executeHealthCheck(ctx, shc.config, target); err != nil {
					// The context is canceled when the dynamic configuration is refreshed.
					if errors.Is(err, context.Canceled) {
						return
					}

					log.Ctx(ctx).Warn().
						Str("targetURL", target.String()).
						Err(err).
						Msg("Health check failed.")

					up = false
					serverUpMetricValue = float64(0)
				}

				shc.balancer.SetStatus(ctx, proxyName, up)

				statusStr := runtime.StatusDown
				if up {
					statusStr = runtime.StatusUp
				}

				shc.info.UpdateServerStatus(target.String(), statusStr)

				shc.metrics.ServiceServerUpGauge().
					With("service", proxyName, "url", target.String()).
					Set(serverUpMetricValue)
			}
		}
	}
}

func (shc *ServiceHealthChecker) executeHealthCheck(ctx context.Context, config *dynamic.ServerHealthCheck, target *url.URL) error {
	ctx, cancel := context.WithDeadline(ctx, time.Now().Add(shc.timeout))
	defer cancel()

	if config.Mode == modeGRPC {
		return shc.checkHealthGRPC(ctx, target)
	}
	return shc.checkHealthHTTP(ctx, target)
}

// checkHealthHTTP returns an error with a meaningful description if the health check failed.
// Dedicated to HTTP servers.
func (shc *ServiceHealthChecker) checkHealthHTTP(ctx context.Context, target *url.URL) error {
	req, err := shc.newRequest(ctx, target)
	if err != nil {
		return fmt.Errorf("create HTTP request: %w", err)
	}

	resp, err := shc.client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}

	defer resp.Body.Close()

	if shc.config.Status == 0 && (resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest) {
		return fmt.Errorf("received error status code: %v", resp.StatusCode)
	}

	if shc.config.Status != 0 && shc.config.Status != resp.StatusCode {
		return fmt.Errorf("received error status code: %v expected status code: %v", resp.StatusCode, shc.config.Status)
	}

	return nil
}

func (shc *ServiceHealthChecker) newRequest(ctx context.Context, target *url.URL) (*http.Request, error) {
	u, err := target.Parse(shc.config.Path)
	if err != nil {
		return nil, err
	}

	if len(shc.config.Scheme) > 0 {
		u.Scheme = shc.config.Scheme
	}

	if shc.config.Port != 0 {
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(shc.config.Port))
	}

	req, err := http.NewRequestWithContext(ctx, shc.config.Method, u.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	if shc.config.Hostname != "" {
		req.Host = shc.config.Hostname
	}

	for k, v := range shc.config.Headers {
		req.Header.Set(k, v)
	}

	return req, nil
}

// checkHealthGRPC returns an error with a meaningful description if the health check failed.
// Dedicated to gRPC servers implementing gRPC Health Checking Protocol v1.
func (shc *ServiceHealthChecker) checkHealthGRPC(ctx context.Context, serverURL *url.URL) error {
	u, err := serverURL.Parse(shc.config.Path)
	if err != nil {
		return fmt.Errorf("failed to parse server URL: %w", err)
	}

	port := u.Port()
	if shc.config.Port != 0 {
		port = strconv.Itoa(shc.config.Port)
	}

	serverAddr := net.JoinHostPort(u.Hostname(), port)

	var opts []grpc.DialOption
	switch shc.config.Scheme {
	case "http", "h2c", "":
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.DialContext(ctx, serverAddr, opts...)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("fail to connect to %s within %s: %w", serverAddr, shc.config.Timeout, err)
		}
		return fmt.Errorf("fail to connect to %s: %w", serverAddr, err)
	}
	defer func() { _ = conn.Close() }()

	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		if stat, ok := status.FromError(err); ok {
			switch stat.Code() {
			case codes.Unimplemented:
				return fmt.Errorf("gRPC server does not implement the health protocol: %w", err)
			case codes.DeadlineExceeded:
				return fmt.Errorf("gRPC health check timeout: %w", err)
			case codes.Canceled:
				return context.Canceled
			}
		}

		return fmt.Errorf("gRPC health check failed: %w", err)
	}

	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("received gRPC status code: %v", resp.GetStatus())
	}

	return nil
}

package healthcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ptypes "github.com/traefik/paerser/types"
	"github.com/traefik/traefik/v3/pkg/config/dynamic"
	"github.com/traefik/traefik/v3/pkg/config/runtime"
	"github.com/traefik/traefik/v3/pkg/testhelpers"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const delta float64 = 1e-10

func TestNewServiceHealthChecker_durations(t *testing.T) {
	testCases := []struct {
		desc        string
		config      *dynamic.ServerHealthCheck
		expInterval time.Duration
		expTimeout  time.Duration
	}{
		{
			desc:        "default values",
			config:      &dynamic.ServerHealthCheck{},
			expInterval: time.Duration(dynamic.DefaultHealthCheckInterval),
			expTimeout:  time.Duration(dynamic.DefaultHealthCheckTimeout),
		},
		{
			desc: "out of range values",
			config: &dynamic.ServerHealthCheck{
				Interval: ptypes.Duration(-time.Second),
				Timeout:  ptypes.Duration(-time.Second),
			},
			expInterval: time.Duration(dynamic.DefaultHealthCheckInterval),
			expTimeout:  time.Duration(dynamic.DefaultHealthCheckTimeout),
		},
		{
			desc: "custom durations",
			config: &dynamic.ServerHealthCheck{
				Interval: ptypes.Duration(time.Second * 10),
				Timeout:  ptypes.Duration(time.Second * 5),
			},
			expInterval: time.Second * 10,
			expTimeout:  time.Second * 5,
		},
		{
			desc: "interval shorter than timeout",
			config: &dynamic.ServerHealthCheck{
				Interval: ptypes.Duration(time.Second),
				Timeout:  ptypes.Duration(time.Second * 5),
			},
			expInterval: time.Second,
			expTimeout:  time.Second * 5,
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			healthChecker := NewServiceHealthChecker(context.Background(), nil, test.config, nil, nil, http.DefaultTransport, nil)
			assert.Equal(t, test.expInterval, healthChecker.interval)
			assert.Equal(t, test.expTimeout, healthChecker.timeout)
		})
	}
}

func TestServiceHealthChecker_newRequest(t *testing.T) {
	testCases := []struct {
		desc        string
		targetURL   string
		config      dynamic.ServerHealthCheck
		expTarget   string
		expError    bool
		expHostname string
		expHeader   string
		expMethod   string
	}{
		{
			desc:      "no port override",
			targetURL: "http://backend1:80",
			config: dynamic.ServerHealthCheck{
				Path: "/test",
				Port: 0,
			},
			expError:    false,
			expTarget:   "http://backend1:80/test",
			expHostname: "backend1:80",
			expMethod:   http.MethodGet,
		},
		{
			desc:      "port override",
			targetURL: "http://backend2:80",
			config: dynamic.ServerHealthCheck{
				Path: "/test",
				Port: 8080,
			},
			expError:    false,
			expTarget:   "http://backend2:8080/test",
			expHostname: "backend2:8080",
			expMethod:   http.MethodGet,
		},
		{
			desc:      "no port override with no port in server URL",
			targetURL: "http://backend1",
			config: dynamic.ServerHealthCheck{
				Path: "/health",
				Port: 0,
			},
			expError:    false,
			expTarget:   "http://backend1/health",
			expHostname: "backend1",
			expMethod:   http.MethodGet,
		},
		{
			desc:      "port override with no port in server URL",
			targetURL: "http://backend2",
			config: dynamic.ServerHealthCheck{
				Path: "/health",
				Port: 8080,
			},
			expError:    false,
			expTarget:   "http://backend2:8080/health",
			expHostname: "backend2:8080",
			expMethod:   http.MethodGet,
		},
		{
			desc:      "scheme override",
			targetURL: "https://backend1:80",
			config: dynamic.ServerHealthCheck{
				Scheme: "http",
				Path:   "/test",
				Port:   0,
			},
			expError:    false,
			expTarget:   "http://backend1:80/test",
			expHostname: "backend1:80",
			expMethod:   http.MethodGet,
		},
		{
			desc:      "path with param",
			targetURL: "http://backend1:80",
			config: dynamic.ServerHealthCheck{
				Path: "/health?powpow=do",
				Port: 0,
			},
			expError:    false,
			expTarget:   "http://backend1:80/health?powpow=do",
			expHostname: "backend1:80",
			expMethod:   http.MethodGet,
		},
		{
			desc:      "path with params",
			targetURL: "http://backend1:80",
			config: dynamic.ServerHealthCheck{
				Path: "/health?powpow=do&do=powpow",
				Port: 0,
			},
			expError:    false,
			expTarget:   "http://backend1:80/health?powpow=do&do=powpow",
			expHostname: "backend1:80",
			expMethod:   http.MethodGet,
		},
		{
			desc:      "path with invalid path",
			targetURL: "http://backend1:80",
			config: dynamic.ServerHealthCheck{
				Path: ":",
				Port: 0,
			},
			expError:    true,
			expTarget:   "",
			expHostname: "backend1:80",
			expMethod:   http.MethodGet,
		},
		{
			desc:      "override hostname",
			targetURL: "http://backend1:80",
			config: dynamic.ServerHealthCheck{
				Hostname: "myhost",
				Path:     "/",
			},
			expTarget:   "http://backend1:80/",
			expHostname: "myhost",
			expHeader:   "",
			expMethod:   http.MethodGet,
		},
		{
			desc:      "not override hostname",
			targetURL: "http://backend1:80",
			config: dynamic.ServerHealthCheck{
				Hostname: "",
				Path:     "/",
			},
			expTarget:   "http://backend1:80/",
			expHostname: "backend1:80",
			expHeader:   "",
			expMethod:   http.MethodGet,
		},
		{
			desc:      "custom header",
			targetURL: "http://backend1:80",
			config: dynamic.ServerHealthCheck{
				Headers:  map[string]string{"Custom-Header": "foo"},
				Hostname: "",
				Path:     "/",
			},
			expTarget:   "http://backend1:80/",
			expHostname: "backend1:80",
			expHeader:   "foo",
			expMethod:   http.MethodGet,
		},
		{
			desc:      "custom header with hostname override",
			targetURL: "http://backend1:80",
			config: dynamic.ServerHealthCheck{
				Headers:  map[string]string{"Custom-Header": "foo"},
				Hostname: "myhost",
				Path:     "/",
			},
			expTarget:   "http://backend1:80/",
			expHostname: "myhost",
			expHeader:   "foo",
			expMethod:   http.MethodGet,
		},
		{
			desc:      "custom method",
			targetURL: "http://backend1:80",
			config: dynamic.ServerHealthCheck{
				Path:   "/",
				Method: http.MethodHead,
			},
			expTarget:   "http://backend1:80/",
			expHostname: "backend1:80",
			expMethod:   http.MethodHead,
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			t.Parallel()

			shc := ServiceHealthChecker{config: &test.config}

			u := testhelpers.MustParseURL(test.targetURL)
			req, err := shc.newRequest(context.Background(), u)

			if test.expError {
				require.Error(t, err)
				assert.Nil(t, req)
			} else {
				require.NoError(t, err, "failed to create new request")
				require.NotNil(t, req)

				assert.Equal(t, test.expTarget, req.URL.String())
				assert.Equal(t, test.expHeader, req.Header.Get("Custom-Header"))
				assert.Equal(t, test.expHostname, req.Host)
				assert.Equal(t, test.expMethod, req.Method)
			}
		})
	}
}

func TestServiceHealthChecker_checkHealthHTTP_NotFollowingRedirects(t *testing.T) {
	redirectServerCalled := false
	redirectTestServer := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		redirectServerCalled = true
	}))
	defer redirectTestServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(dynamic.DefaultHealthCheckTimeout))
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Add("location", redirectTestServer.URL)
		rw.WriteHeader(http.StatusSeeOther)
	}))
	defer server.Close()

	config := &dynamic.ServerHealthCheck{
		Path:            "/path",
		FollowRedirects: Bool(false),
		Interval:        dynamic.DefaultHealthCheckInterval,
		Timeout:         dynamic.DefaultHealthCheckTimeout,
	}
	healthChecker := NewServiceHealthChecker(ctx, nil, config, nil, nil, http.DefaultTransport, nil)

	err := healthChecker.checkHealthHTTP(ctx, testhelpers.MustParseURL(server.URL))
	require.NoError(t, err)

	assert.False(t, redirectServerCalled, "HTTP redirect must not be followed")
}

func TestServiceHealthChecker_Launch(t *testing.T) {
	testCases := []struct {
		desc                  string
		mode                  string
		status                int
		server                StartTestServer
		expNumRemovedServers  int
		expNumUpsertedServers int
		expGaugeValue         float64
		targetStatus          string
	}{
		{
			desc:                  "healthy server staying healthy",
			server:                newHTTPServer(http.StatusOK),
			expNumRemovedServers:  0,
			expNumUpsertedServers: 1,
			expGaugeValue:         1,
			targetStatus:          runtime.StatusUp,
		},
		{
			desc:                  "healthy server staying healthy, with custom code status check",
			server:                newHTTPServer(http.StatusNotFound),
			status:                http.StatusNotFound,
			expNumRemovedServers:  0,
			expNumUpsertedServers: 1,
			expGaugeValue:         1,
			targetStatus:          runtime.StatusUp,
		},
		{
			desc:                  "healthy server staying healthy (StatusNoContent)",
			server:                newHTTPServer(http.StatusNoContent),
			expNumRemovedServers:  0,
			expNumUpsertedServers: 1,
			expGaugeValue:         1,
			targetStatus:          runtime.StatusUp,
		},
		{
			desc:                  "healthy server staying healthy (StatusPermanentRedirect)",
			server:                newHTTPServer(http.StatusPermanentRedirect),
			expNumRemovedServers:  0,
			expNumUpsertedServers: 1,
			expGaugeValue:         1,
			targetStatus:          runtime.StatusUp,
		},
		{
			desc:                  "healthy server becoming sick",
			server:                newHTTPServer(http.StatusServiceUnavailable),
			expNumRemovedServers:  1,
			expNumUpsertedServers: 0,
			expGaugeValue:         0,
			targetStatus:          runtime.StatusDown,
		},
		{
			desc:                  "healthy server becoming sick, with custom code status check",
			server:                newHTTPServer(http.StatusOK),
			status:                http.StatusServiceUnavailable,
			expNumRemovedServers:  1,
			expNumUpsertedServers: 0,
			expGaugeValue:         0,
			targetStatus:          runtime.StatusDown,
		},
		{
			desc:                  "healthy server toggling to sick and back to healthy",
			server:                newHTTPServer(http.StatusServiceUnavailable, http.StatusOK),
			expNumRemovedServers:  1,
			expNumUpsertedServers: 1,
			expGaugeValue:         1,
			targetStatus:          runtime.StatusUp,
		},
		{
			desc:                  "healthy server toggling to healthy and go to sick",
			server:                newHTTPServer(http.StatusOK, http.StatusServiceUnavailable),
			expNumRemovedServers:  1,
			expNumUpsertedServers: 1,
			expGaugeValue:         0,
			targetStatus:          runtime.StatusDown,
		},
		{
			desc:                  "healthy grpc server staying healthy",
			mode:                  "grpc",
			server:                newGRPCServer(healthpb.HealthCheckResponse_SERVING),
			expNumRemovedServers:  0,
			expNumUpsertedServers: 1,
			expGaugeValue:         1,
			targetStatus:          runtime.StatusUp,
		},
		{
			desc:                  "healthy grpc server becoming sick",
			mode:                  "grpc",
			server:                newGRPCServer(healthpb.HealthCheckResponse_NOT_SERVING),
			expNumRemovedServers:  1,
			expNumUpsertedServers: 0,
			expGaugeValue:         0,
			targetStatus:          runtime.StatusDown,
		},
		{
			desc:                  "healthy grpc server toggling to sick and back to healthy",
			mode:                  "grpc",
			server:                newGRPCServer(healthpb.HealthCheckResponse_NOT_SERVING, healthpb.HealthCheckResponse_SERVING),
			expNumRemovedServers:  1,
			expNumUpsertedServers: 1,
			expGaugeValue:         1,
			targetStatus:          runtime.StatusUp,
		},
	}

	for _, test := range testCases {
		test := test
		t.Run(test.desc, func(t *testing.T) {
			t.Parallel()

			// The context is passed to the health check and
			// canonically canceled by the test server once all expected requests have been received.
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			targetURL, timeout := test.server.Start(t, cancel)

			lb := &testLoadBalancer{RWMutex: &sync.RWMutex{}}

			config := &dynamic.ServerHealthCheck{
				Mode:     test.mode,
				Status:   test.status,
				Path:     "/path",
				Interval: ptypes.Duration(500 * time.Millisecond),
				Timeout:  ptypes.Duration(499 * time.Millisecond),
			}

			gauge := &testhelpers.CollectingGauge{}
			serviceInfo := &runtime.ServiceInfo{}
			hc := NewServiceHealthChecker(ctx, &MetricsMock{gauge}, config, lb, serviceInfo, http.DefaultTransport, map[string]*url.URL{"test": targetURL})

			wg := sync.WaitGroup{}
			wg.Add(1)

			go func() {
				hc.Launch(ctx)
				wg.Done()
			}()

			select {
			case <-time.After(timeout):
				t.Fatal("test did not complete in time")
			case <-ctx.Done():
				wg.Wait()
			}

			lb.Lock()
			defer lb.Unlock()

			assert.Equal(t, test.expNumRemovedServers, lb.numRemovedServers, "removed servers")
			assert.Equal(t, test.expNumUpsertedServers, lb.numUpsertedServers, "upserted servers")
			assert.InDelta(t, test.expGaugeValue, gauge.GaugeValue, delta, "ServerUp Gauge")
			assert.Equal(t, map[string]string{targetURL.String(): test.targetStatus}, serviceInfo.GetAllStatus())
		})
	}
}

func TestNewRequest(t *testing.T) {
	type expected struct {
		err   bool
		value string
	}

	testCases := []struct {
		desc      string
		serverURL string
		options   Options
		expected  expected
	}{
		{
			desc:      "no port override",
			serverURL: "http://backend1:80",
			options: Options{
				Path: "/test",
				Port: 0,
			},
			expected: expected{
				err:   false,
				value: "http://backend1:80/test",
			},
		},
		{
			desc:      "port override",
			serverURL: "http://backend2:80",
			options: Options{
				Path: "/test",
				Port: 8080,
			},
			expected: expected{
				err:   false,
				value: "http://backend2:8080/test",
			},
		},
		{
			desc:      "no port override with no port in server URL",
			serverURL: "http://backend1",
			options: Options{
				Path: "/health",
				Port: 0,
			},
			expected: expected{
				err:   false,
				value: "http://backend1/health",
			},
		},
		{
			desc:      "port override with no port in server URL",
			serverURL: "http://backend2",
			options: Options{
				Path: "/health",
				Port: 8080,
			},
			expected: expected{
				err:   false,
				value: "http://backend2:8080/health",
			},
		},
		{
			desc:      "scheme override",
			serverURL: "https://backend1:80",
			options: Options{
				Scheme: "http",
				Path:   "/test",
				Port:   0,
			},
			expected: expected{
				err:   false,
				value: "http://backend1:80/test",
			},
		},
		{
			desc:      "path with param",
			serverURL: "http://backend1:80",
			options: Options{
				Path: "/health?powpow=do",
				Port: 0,
			},
			expected: expected{
				err:   false,
				value: "http://backend1:80/health?powpow=do",
			},
		},
		{
			desc:      "path with params",
			serverURL: "http://backend1:80",
			options: Options{
				Path: "/health?powpow=do&do=powpow",
				Port: 0,
			},
			expected: expected{
				err:   false,
				value: "http://backend1:80/health?powpow=do&do=powpow",
			},
		},
		{
			desc:      "path with invalid path",
			serverURL: "http://backend1:80",
			options: Options{
				Path: ":",
				Port: 0,
			},
			expected: expected{
				err:   true,
				value: "",
			},
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			t.Parallel()

			backend := NewBackendConfig(test.options, "backendName")

			u := testhelpers.MustParseURL(test.serverURL)

			req, err := backend.newRequest(u)

			if test.expected.err {
				require.Error(t, err)
				assert.Nil(t, nil)
			} else {
				require.NoError(t, err, "failed to create new backend request")
				require.NotNil(t, req)
				assert.Equal(t, test.expected.value, req.URL.String())
			}
		})
	}
}

func TestRequestOptions(t *testing.T) {
	testCases := []struct {
		desc             string
		serverURL        string
		options          Options
		expectedHostname string
		expectedHeader   string
		expectedMethod   string
	}{
		{
			desc:      "override hostname",
			serverURL: "http://backend1:80",
			options: Options{
				Hostname: "myhost",
				Path:     "/",
			},
			expectedHostname: "myhost",
			expectedHeader:   "",
			expectedMethod:   http.MethodGet,
		},
		{
			desc:      "not override hostname",
			serverURL: "http://backend1:80",
			options: Options{
				Hostname: "",
				Path:     "/",
			},
			expectedHostname: "backend1:80",
			expectedHeader:   "",
			expectedMethod:   http.MethodGet,
		},
		{
			desc:      "custom header",
			serverURL: "http://backend1:80",
			options: Options{
				Headers:  map[string]string{"Custom-Header": "foo"},
				Hostname: "",
				Path:     "/",
			},
			expectedHostname: "backend1:80",
			expectedHeader:   "foo",
			expectedMethod:   http.MethodGet,
		},
		{
			desc:      "custom header with hostname override",
			serverURL: "http://backend1:80",
			options: Options{
				Headers:  map[string]string{"Custom-Header": "foo"},
				Hostname: "myhost",
				Path:     "/",
			},
			expectedHostname: "myhost",
			expectedHeader:   "foo",
			expectedMethod:   http.MethodGet,
		},
		{
			desc:      "custom method",
			serverURL: "http://backend1:80",
			options: Options{
				Path:   "/",
				Method: http.MethodHead,
			},
			expectedHostname: "backend1:80",
			expectedMethod:   http.MethodHead,
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			t.Parallel()

			backend := NewBackendConfig(test.options, "backendName")

			u, err := url.Parse(test.serverURL)
			require.NoError(t, err)

			req, err := backend.newRequest(u)
			require.NoError(t, err, "failed to create new backend request")

			req = backend.setRequestOptions(req)

			assert.Equal(t, "http://backend1:80/", req.URL.String())
			assert.Equal(t, test.expectedHostname, req.Host)
			assert.Equal(t, test.expectedHeader, req.Header.Get("Custom-Header"))
			assert.Equal(t, test.expectedMethod, req.Method)
		})
	}
}

func TestBalancers_Servers(t *testing.T) {
	server1, err := url.Parse("http://foo.com")
	require.NoError(t, err)

	balancer1, err := roundrobin.New(nil)
	require.NoError(t, err)

	err = balancer1.UpsertServer(server1)
	require.NoError(t, err)

	server2, err := url.Parse("http://foo.com")
	require.NoError(t, err)

	balancer2, err := roundrobin.New(nil)
	require.NoError(t, err)

	err = balancer2.UpsertServer(server2)
	require.NoError(t, err)

	balancers := Balancers([]Balancer{balancer1, balancer2})

	want, err := url.Parse("http://foo.com")
	require.NoError(t, err)

	assert.Len(t, balancers.Servers(), 1)
	assert.Equal(t, want, balancers.Servers()[0])
}

func TestBalancers_UpsertServer(t *testing.T) {
	balancer1, err := roundrobin.New(nil)
	require.NoError(t, err)

	balancer2, err := roundrobin.New(nil)
	require.NoError(t, err)

	want, err := url.Parse("http://foo.com")
	require.NoError(t, err)

	balancers := Balancers([]Balancer{balancer1, balancer2})

	err = balancers.UpsertServer(want)
	require.NoError(t, err)

	assert.Len(t, balancer1.Servers(), 1)
	assert.Equal(t, want, balancer1.Servers()[0])

	assert.Len(t, balancer2.Servers(), 1)
	assert.Equal(t, want, balancer2.Servers()[0])
}

func TestBalancers_RemoveServer(t *testing.T) {
	server, err := url.Parse("http://foo.com")
	require.NoError(t, err)

	balancer1, err := roundrobin.New(nil)
	require.NoError(t, err)

	err = balancer1.UpsertServer(server)
	require.NoError(t, err)

	balancer2, err := roundrobin.New(nil)
	require.NoError(t, err)

	err = balancer2.UpsertServer(server)
	require.NoError(t, err)

	balancers := Balancers([]Balancer{balancer1, balancer2})

	err = balancers.RemoveServer(server)
	require.NoError(t, err)

	assert.Empty(t, balancer1.Servers())
	assert.Empty(t, balancer2.Servers())
}

type testLoadBalancer struct {
	// RWMutex needed due to parallel test execution: Both the system-under-test
	// and the test assertions reference the counters.
	*sync.RWMutex
	numRemovedServers  int
	numUpsertedServers int
	servers            []*url.URL
	// options is just to make sure that LBStatusUpdater forwards options on Upsert to its BalancerHandler
	options []roundrobin.ServerOption
}

func (lb *testLoadBalancer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// noop
}

func (lb *testLoadBalancer) RemoveServer(u *url.URL) error {
	lb.Lock()
	defer lb.Unlock()
	lb.numRemovedServers++
	lb.removeServer(u)
	return nil
}

func (lb *testLoadBalancer) UpsertServer(u *url.URL, options ...roundrobin.ServerOption) error {
	lb.Lock()
	defer lb.Unlock()
	lb.numUpsertedServers++
	lb.servers = append(lb.servers, u)
	lb.options = append(lb.options, options...)
	return nil
}

func (lb *testLoadBalancer) Servers() []*url.URL {
	return lb.servers
}

func (lb *testLoadBalancer) Options() []roundrobin.ServerOption {
	return lb.options
}

func (lb *testLoadBalancer) removeServer(u *url.URL) {
	var i int
	var serverURL *url.URL
	found := false
	for i, serverURL = range lb.servers {
		if *serverURL == *u {
			found = true
			break
		}
	}
	if !found {
		return
	}

	lb.servers = append(lb.servers[:i], lb.servers[i+1:]...)
}

func newTestServer(done func(), healthSequence []int) *httptest.Server {
	handler := &testHandler{
		done:           done,
		healthSequence: healthSequence,
	}
	return httptest.NewServer(handler)
}

// ServeHTTP returns HTTP response codes following a status sequences.
// It calls the given 'done' function once all request health indicators have been depleted.
func (th *testHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if len(th.healthSequence) == 0 {
		panic("received unexpected request")
	}

	w.WriteHeader(th.healthSequence[0])

	th.healthSequence = th.healthSequence[1:]
	if len(th.healthSequence) == 0 {
		th.done()
	}
}

func TestLBStatusUpdater(t *testing.T) {
	lb := &testLoadBalancer{RWMutex: &sync.RWMutex{}}
	svInfo := &runtime.ServiceInfo{}
	lbsu := NewLBStatusUpdater(lb, svInfo, nil)
	newServer, err := url.Parse("http://foo.com")
	assert.NoError(t, err)
	err = lbsu.UpsertServer(newServer, roundrobin.Weight(1))
	assert.NoError(t, err)
	assert.Len(t, lbsu.Servers(), 1)
	assert.Len(t, lbsu.BalancerHandler.(*testLoadBalancer).Options(), 1)
	statuses := svInfo.GetAllStatus()
	assert.Len(t, statuses, 1)
	for k, v := range statuses {
		assert.Equal(t, newServer.String(), k)
		assert.Equal(t, serverUp, v)
		break
	}
	err = lbsu.RemoveServer(newServer)
	assert.NoError(t, err)
	assert.Empty(t, lbsu.Servers())
	statuses = svInfo.GetAllStatus()
	assert.Len(t, statuses, 1)
	for k, v := range statuses {
		assert.Equal(t, newServer.String(), k)
		assert.Equal(t, serverDown, v)
		break
	}
}

func TestNotFollowingRedirects(t *testing.T) {
	redirectServerCalled := false
	redirectTestServer := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		redirectServerCalled = true
	}))
	defer redirectTestServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Add("location", redirectTestServer.URL)
		rw.WriteHeader(http.StatusSeeOther)
		cancel()
	}))
	defer server.Close()

	lb := &testLoadBalancer{
		RWMutex: &sync.RWMutex{},
		servers: []*url.URL{testhelpers.MustParseURL(server.URL)},
	}

	backend := NewBackendConfig(Options{
		Path:            "/path",
		Interval:        healthCheckInterval,
		Timeout:         healthCheckTimeout,
		LB:              lb,
		FollowRedirects: false,
	}, "backendName")

	collectingMetrics := &testhelpers.CollectingGauge{}
	check := HealthCheck{
		Backends: make(map[string]*BackendConfig),
		metrics:  metricsHealthcheck{serverUpGauge: collectingMetrics},
	}

	wg := sync.WaitGroup{}
	wg.Add(1)

	go func() {
		check.execute(ctx, backend)
		wg.Done()
	}()

	timeout := time.Duration(int(healthCheckInterval) + 500)
	select {
	case <-time.After(timeout):
		t.Fatal("test did not complete in time")
	case <-ctx.Done():
		wg.Wait()
	}

	assert.False(t, redirectServerCalled, "HTTP redirect must not be followed")
}

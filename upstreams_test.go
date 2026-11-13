package caddy_docker_upstreams

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestCaddyModule(t *testing.T) {
	info := Upstreams{}.CaddyModule()

	assert.Equal(t, "http.reverse_proxy.upstreams.docker", string(info.ID))
	assert.IsType(t, &Upstreams{}, info.New())
}

func TestGetUpstreams(t *testing.T) {
	const (
		port         = "8080"
		apiAddr      = "10.0.0.1"
		webAddr      = "10.0.0.2"
		catchAllAddr = "10.0.0.3"
	)
	var (
		apiUpstream      = net.JoinHostPort(apiAddr, port)
		webUpstream      = net.JoinHostPort(webAddr, port)
		catchAllUpstream = net.JoinHostPort(catchAllAddr, port)
	)

	host := caddyhttp.MatchHost{"example.com"}
	apiPath := caddyhttp.MatchPath{"/api/*"}

	var u Upstreams
	withCandidates(t, &u)
	u.candidates = []candidate{
		{matchers: caddyhttp.MatcherSet{&host, &apiPath}, address: apiAddr, port: port},
		{matchers: caddyhttp.MatcherSet{&host}, address: webAddr, port: port},
		{matchers: caddyhttp.MatcherSet{}, address: catchAllAddr, port: port},
	}

	t.Run("matches host and path", func(t *testing.T) {
		req := prepareRequest(mustRequest(http.MethodGet, "http://example.com/api/users"))
		got, err := u.GetUpstreams(req)
		require.NoError(t, err)
		// api (host+path), web (host), catch-all (empty) all match.
		assert.ElementsMatch(t, []string{apiUpstream, webUpstream, catchAllUpstream}, upstreamDials(got))
	})

	t.Run("matches host only", func(t *testing.T) {
		req := prepareRequest(mustRequest(http.MethodGet, "http://example.com/web"))
		got, err := u.GetUpstreams(req)
		require.NoError(t, err)
		// api does not match (wrong path); web and catch-all match.
		assert.ElementsMatch(t, []string{webUpstream, catchAllUpstream}, upstreamDials(got))
	})

	t.Run("matches catch-all only", func(t *testing.T) {
		req := prepareRequest(mustRequest(http.MethodGet, "http://other.com/api/users"))
		got, err := u.GetUpstreams(req)
		require.NoError(t, err)
		// Only the empty matcher set matches a different host.
		assert.ElementsMatch(t, []string{catchAllUpstream}, upstreamDials(got))
	})
}

func TestGetUpstreamsLabelSelector(t *testing.T) {
	const (
		port        = "8080"
		firstAddr   = "10.0.0.1"
		secondAddr  = "10.0.0.2"
		otherAddr   = "10.0.0.3"
		noLabelAddr = "10.0.0.4"
	)
	var (
		firstDial  = net.JoinHostPort(firstAddr, port)
		secondDial = net.JoinHostPort(secondAddr, port)
	)

	candidates := []candidate{
		{labels: map[string]string{"com.docker.compose.service": "first"}, address: firstAddr, port: port},
		{labels: map[string]string{"com.docker.compose.service": "second"}, address: secondAddr, port: port},
		{labels: map[string]string{"com.docker.compose.service": "other"}, address: otherAddr, port: port},
		{labels: nil, address: noLabelAddr, port: port},
	}

	req := prepareRequest(mustRequest(http.MethodGet, "http://example.com/"))

	t.Run("empty selector matches all", func(t *testing.T) {
		u := Upstreams{candidates: candidates}
		got, err := u.GetUpstreams(req)
		require.NoError(t, err)
		assert.Len(t, got, 4)
	})

	t.Run("selects a single service", func(t *testing.T) {
		u := Upstreams{Labels: map[string][]string{
			"com.docker.compose.service": {"first"},
		}, candidates: candidates}
		got, err := u.GetUpstreams(req)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{firstDial}, upstreamDials(got))
	})

	t.Run("value list is ORed", func(t *testing.T) {
		u := Upstreams{Labels: map[string][]string{
			"com.docker.compose.service": {"first", "second"},
		}, candidates: candidates}
		got, err := u.GetUpstreams(req)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{firstDial, secondDial}, upstreamDials(got))
	})

	t.Run("keys are ANDed", func(t *testing.T) {
		u := Upstreams{Labels: map[string][]string{
			"com.docker.compose.service": {"first"},
			"missing.label":              {"whatever"},
		}, candidates: candidates}
		got, err := u.GetUpstreams(req)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func upstreamDials(ups []*reverseproxy.Upstream) []string {
	out := make([]string, len(ups))
	for i, up := range ups {
		out[i] = up.Dial
	}
	return out
}

// TestGetUpstreamsPortIsolatedPerInstance verifies that blocks configured with
// different ports retain independent candidate lists and dial their own port.
func TestGetUpstreamsPortIsolatedPerInstance(t *testing.T) {
	ctx := newTestContext(t)

	// Two containers telling themselves apart by a service label; neither
	// carries a port label, so each site's port comes from its `port`
	// directive — the same image on two different ports.
	cli := &mockDockerClient{}
	cli.On("ContainerList", mock.Anything, mock.Anything).Return(
		client.ContainerListResult{Items: []container.Summary{
			summary("alpha",
				map[string]string{"com.docker.compose.service": "alpha"},
				map[string]string{"bridge": "10.0.0.1"},
			),
			summary("beta",
				map[string]string{"com.docker.compose.service": "beta"},
				map[string]string{"bridge": "10.0.0.2"},
			),
		}}, nil)

	alpha := &Upstreams{Port: "5001", Labels: map[string][]string{"com.docker.compose.service": {"alpha"}}}
	beta := &Upstreams{Port: "5002", Labels: map[string][]string{"com.docker.compose.service": {"beta"}}}
	withCandidates(t, alpha)
	withCandidates(t, beta)

	// Provision each block independently; each stores its own candidates.
	require.NoError(t, alpha.provisionCandidates(ctx, cli))
	require.NoError(t, beta.provisionCandidates(ctx, cli))

	req := newRequest(t, http.MethodGet, "http://localhost/")

	gotAlpha, err := alpha.GetUpstreams(req)
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.1:5001"}, upstreamDials(gotAlpha))

	gotBeta, err := beta.GetUpstreams(req)
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.2:5002"}, upstreamDials(gotBeta))
}

// TestGetUpstreamsPortResolution covers how the effective upstream port is
// chosen: the port directive takes precedence over the container's port label,
// the label is used when no directive is set, and a candidate with neither is
// dropped.
func TestGetUpstreamsPortResolution(t *testing.T) {
	tests := []struct {
		name      string
		port      string // the block's port directive
		candidate candidate
		wantDials []string
	}{
		{
			name:      "directive overrides label",
			port:      "8080",
			candidate: candidate{address: "10.0.0.1", port: "9090"},
			wantDials: []string{"10.0.0.1:8080"},
		},
		{
			name:      "directive makes label optional",
			port:      "8080",
			candidate: candidate{address: "10.0.0.1"},
			wantDials: []string{"10.0.0.1:8080"},
		},
		{
			name:      "label used when no directive",
			candidate: candidate{address: "10.0.0.1", port: "9090"},
			wantDials: []string{"10.0.0.1:9090"},
		},
		{
			name:      "no port anywhere is dropped",
			candidate: candidate{address: "10.0.0.1"},
			wantDials: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := &Upstreams{Port: tt.port}
			withCandidates(t, u)
			u.candidates = []candidate{tt.candidate}

			got, err := u.GetUpstreams(newRequest(t, http.MethodGet, "http://example.com/"))
			require.NoError(t, err)
			assert.Equal(t, tt.wantDials, upstreamDials(got))
		})
	}
}

func mustRequest(method, target string) *http.Request {
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		panic(err)
	}
	return req
}

// mockDockerClient is a testify mock implementing dockerClient, shared by the
// provisionCandidates and keepUpdated tests. It also tracks the number of
// Events calls with an atomic counter so tests can poll it race-free while
// keepUpdated runs in another goroutine.
type mockDockerClient struct {
	mock.Mock
	eventsCalls atomic.Int64
}

func (m *mockDockerClient) ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	args := m.Called(ctx, options)
	return args.Get(0).(client.ContainerListResult), args.Error(1)
}

func (m *mockDockerClient) Events(ctx context.Context, options client.EventsListOptions) client.EventsResult {
	m.eventsCalls.Add(1)
	return m.Called(ctx, options).Get(0).(client.EventsResult)
}

func (m *mockDockerClient) Close() error {
	return m.Called().Error(0)
}

// eventStream is one connection's worth of channels: the test keeps the send
// side, keepUpdated reads the receive side via the EventsResult.
type eventStream struct {
	messages chan events.Message
	errs     chan error
}

func newEventStream() eventStream {
	return eventStream{
		messages: make(chan events.Message, 1),
		errs:     make(chan error, 1),
	}
}

func (s eventStream) result() client.EventsResult {
	return client.EventsResult{Messages: s.messages, Err: s.errs}
}

// newCancelableContext returns a caddy.Context whose cancellation the test
// controls directly.
func newCancelableContext(t *testing.T) (caddy.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	return ctx, cancel
}

// oneContainerResult is a canned ContainerList response with a single valid
// container, so a re-provision produces exactly one candidate.
func oneContainerResult() client.ContainerListResult {
	return client.ContainerListResult{Items: []container.Summary{{
		ID:     "a",
		Labels: map[string]string{LabelUpstreamPort: "8080"},
		NetworkSettings: &container.NetworkSettingsSummary{
			Networks: map[string]*network.EndpointSettings{
				"bridge": {IPAddress: netip.MustParseAddr("10.0.0.1")},
			},
		},
	}}}
}

func candidateCount(u *Upstreams) int {
	u.candidatesMu.RLock()
	defer u.candidatesMu.RUnlock()
	return len(u.candidates)
}

// awaitReturn fails the test if keepUpdated has not returned (done closed)
// within the timeout.
func awaitReturn(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("keepUpdated did not return")
	}
}

func TestKeepUpdatedReprovisionsOnEvent(t *testing.T) {
	u := &Upstreams{debounceInterval: time.Millisecond, reconnectDelay: time.Millisecond}
	withCandidates(t, u)
	ctx, _ := newCancelableContext(t)

	stream := newEventStream()
	cli := &mockDockerClient{}
	cli.On("Events", mock.Anything, mock.Anything).Return(stream.result())
	cli.On("ContainerList", mock.Anything, mock.Anything).Return(oneContainerResult(), nil)
	cli.On("Close").Return(nil)

	done := make(chan struct{})
	go func() {
		u.keepUpdated(ctx, cli)
		close(done)
	}()

	// A container event must trigger a re-provision.
	stream.messages <- events.Message{}
	require.Eventually(t, func() bool { return candidateCount(u) == 1 }, 2*time.Second, time.Millisecond)

	// A canceled error stops the loop.
	stream.errs <- context.Canceled
	awaitReturn(t, done)

	cli.AssertCalled(t, "Close")
	cli.AssertExpectations(t)
}

func TestKeepUpdatedStopsOnCanceled(t *testing.T) {
	u := &Upstreams{debounceInterval: time.Millisecond, reconnectDelay: time.Millisecond}
	withCandidates(t, u)
	ctx, _ := newCancelableContext(t)

	stream := newEventStream()
	cli := &mockDockerClient{}
	cli.On("Events", mock.Anything, mock.Anything).Return(stream.result())
	cli.On("Close").Return(nil)

	done := make(chan struct{})
	go func() {
		u.keepUpdated(ctx, cli)
		close(done)
	}()

	stream.errs <- context.Canceled
	awaitReturn(t, done)

	cli.AssertCalled(t, "Close")
	// No event was delivered, so the container list is never fetched.
	cli.AssertNotCalled(t, "ContainerList", mock.Anything, mock.Anything)
	cli.AssertExpectations(t)
}

func TestKeepUpdatedReconnectsAfterError(t *testing.T) {
	u := &Upstreams{debounceInterval: time.Millisecond, reconnectDelay: time.Millisecond}
	withCandidates(t, u)
	ctx, _ := newCancelableContext(t)

	first := newEventStream()
	second := newEventStream()
	cli := &mockDockerClient{}
	// First connection, then a fresh connection after the reconnect delay.
	cli.On("Events", mock.Anything, mock.Anything).Return(first.result()).Once()
	cli.On("Events", mock.Anything, mock.Anything).Return(second.result()).Once()
	cli.On("Close").Return(nil)

	done := make(chan struct{})
	go func() {
		u.keepUpdated(ctx, cli)
		close(done)
	}()

	// A transient (non-canceled) error breaks the inner loop and triggers a
	// reconnect: Events is invoked a second time.
	first.errs <- errors.New("boom")
	require.Eventually(t, func() bool {
		return cli.eventsCalls.Load() == 2
	}, 2*time.Second, time.Millisecond)

	// Stop via the second connection.
	second.errs <- context.Canceled
	awaitReturn(t, done)

	cli.AssertNumberOfCalls(t, "Close", 1)
	cli.AssertExpectations(t)
}

func TestKeepUpdatedReturnsOnContextCancelDuringBackoff(t *testing.T) {
	u := &Upstreams{debounceInterval: time.Millisecond, reconnectDelay: time.Second}
	withCandidates(t, u)
	ctx, cancel := newCancelableContext(t)

	stream := newEventStream()
	cli := &mockDockerClient{}
	cli.On("Events", mock.Anything, mock.Anything).Return(stream.result())
	cli.On("Close").Return(nil)

	done := make(chan struct{})
	go func() {
		u.keepUpdated(ctx, cli)
		close(done)
	}()

	// Break the inner loop with a transient error, then cancel while the loop
	// waits out the reconnect delay.
	stream.errs <- errors.New("boom")
	require.Eventually(t, func() bool {
		return cli.eventsCalls.Load() == 1
	}, 2*time.Second, time.Millisecond)
	cancel()

	awaitReturn(t, done)

	// It must have returned via ctx.Done(), without reconnecting.
	cli.AssertNumberOfCalls(t, "Events", 1)
	cli.AssertCalled(t, "Close")
}

func TestCaddyModuleDefaults(t *testing.T) {
	mod := Upstreams{}.CaddyModule().New()
	u, ok := mod.(*Upstreams)
	require.True(t, ok)
	assert.Equal(t, defaultDebounceInterval, u.debounceInterval)
	assert.Equal(t, defaultReconnectDelay, u.reconnectDelay)
}

// ContainerInspect defaults to an absent health check so existing tests using
// mockDockerClient retain their previous behavior. Health-specific tests can
// override it with healthInspectClient.
func (m *mockDockerClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{}, nil
}

type healthInspectClient struct {
	*mockDockerClient
	inspect func(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
}

func (m *healthInspectClient) ContainerInspect(ctx context.Context, id string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return m.inspect(ctx, id, options)
}

func TestProvisionCandidatesHealthEligibility(t *testing.T) {
	u := &Upstreams{}
	withCandidates(t, u)
	ctx := newTestContext(t)

	ids := []string{"healthy", "unhealthy", "no-check", "disabled", "inspect-error"}
	containers := make([]container.Summary, 0, len(ids))
	for i, id := range ids {
		containers = append(containers, container.Summary{
			ID:     id,
			Labels: map[string]string{LabelUpstreamPort: "8080"},
			NetworkSettings: &container.NetworkSettingsSummary{
				Networks: map[string]*network.EndpointSettings{
					"bridge": {IPAddress: netip.MustParseAddr("10.0.0." + string(rune('1'+i)))},
				},
			},
		})
	}

	base := &mockDockerClient{}
	base.On("ContainerList", mock.Anything, mock.Anything).
		Return(client.ContainerListResult{Items: containers}, nil)

	cli := &healthInspectClient{
		mockDockerClient: base,
		inspect: func(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
			if id == "inspect-error" {
				return client.ContainerInspectResult{}, errors.New("inspect failed")
			}
			if id == "no-check" {
				return client.ContainerInspectResult{}, nil
			}
			test := []string{"CMD-SHELL", "true"}
			if id == "disabled" {
				test = []string{"NONE"}
			}
			status := container.HealthStatus("unhealthy")
			if id == "healthy" {
				status = container.Healthy
			}
			return client.ContainerInspectResult{Container: container.InspectResponse{
				Config: &container.Config{Healthcheck: &container.HealthConfig{Test: test}},
				State:  &container.State{Health: &container.Health{Status: status}},
			}}, nil
		},
	}

	require.NoError(t, u.provisionCandidates(ctx, cli))

	u.candidatesMu.RLock()
	got := dials(u.candidates)
	u.candidatesMu.RUnlock()
	assert.ElementsMatch(t, []string{"10.0.0.1:8080", "10.0.0.3:8080", "10.0.0.4:8080"}, got)
	base.AssertExpectations(t)
}

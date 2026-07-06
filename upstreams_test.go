package caddy_docker_upstreams

import (
	"context"
	"errors"
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
	host := caddyhttp.MatchHost{"example.com"}
	apiUpstream := &reverseproxy.Upstream{Dial: "10.0.0.1:8080"}
	webUpstream := &reverseproxy.Upstream{Dial: "10.0.0.2:8080"}
	catchAllUpstream := &reverseproxy.Upstream{Dial: "10.0.0.3:8080"}

	apiPath := caddyhttp.MatchPath{"/api/*"}

	// Populate the package-level candidates. Restore afterwards so tests stay
	// independent.
	candidatesMu.Lock()
	prev := candidates
	candidates = []candidate{
		{matchers: caddyhttp.MatcherSet{&host, &apiPath}, upstream: apiUpstream},
		{matchers: caddyhttp.MatcherSet{&host}, upstream: webUpstream},
		{matchers: caddyhttp.MatcherSet{}, upstream: catchAllUpstream},
	}
	candidatesMu.Unlock()
	t.Cleanup(func() {
		candidatesMu.Lock()
		candidates = prev
		candidatesMu.Unlock()
	})

	var u Upstreams

	t.Run("matches host and path", func(t *testing.T) {
		req := prepareRequest(mustRequest(http.MethodGet, "http://example.com/api/users"))
		got, err := u.GetUpstreams(req)
		require.NoError(t, err)
		// api (host+path), web (host), catch-all (empty) all match.
		assert.ElementsMatch(t, []*reverseproxy.Upstream{apiUpstream, webUpstream, catchAllUpstream}, got)
	})

	t.Run("matches host only", func(t *testing.T) {
		req := prepareRequest(mustRequest(http.MethodGet, "http://example.com/web"))
		got, err := u.GetUpstreams(req)
		require.NoError(t, err)
		// api does not match (wrong path); web and catch-all match.
		assert.ElementsMatch(t, []*reverseproxy.Upstream{webUpstream, catchAllUpstream}, got)
	})

	t.Run("matches catch-all only", func(t *testing.T) {
		req := prepareRequest(mustRequest(http.MethodGet, "http://other.com/api/users"))
		got, err := u.GetUpstreams(req)
		require.NoError(t, err)
		// Only the empty matcher set matches a different host.
		assert.ElementsMatch(t, []*reverseproxy.Upstream{catchAllUpstream}, got)
	})
}

func TestGetUpstreamsLabelSelector(t *testing.T) {
	first := &reverseproxy.Upstream{Dial: "10.0.0.1:8080"}
	second := &reverseproxy.Upstream{Dial: "10.0.0.2:8080"}
	other := &reverseproxy.Upstream{Dial: "10.0.0.3:8080"}

	candidatesMu.Lock()
	prev := candidates
	candidates = []candidate{
		{labels: map[string]string{"com.docker.compose.service": "first"}, upstream: first},
		{labels: map[string]string{"com.docker.compose.service": "second"}, upstream: second},
		{labels: map[string]string{"com.docker.compose.service": "other"}, upstream: other},
		{labels: nil, upstream: &reverseproxy.Upstream{Dial: "10.0.0.4:8080"}},
	}
	candidatesMu.Unlock()
	t.Cleanup(func() {
		candidatesMu.Lock()
		candidates = prev
		candidatesMu.Unlock()
	})

	req := prepareRequest(mustRequest(http.MethodGet, "http://example.com/"))

	t.Run("empty selector matches all", func(t *testing.T) {
		var u Upstreams
		got, err := u.GetUpstreams(req)
		require.NoError(t, err)
		assert.Len(t, got, 4)
	})

	t.Run("selects a single service", func(t *testing.T) {
		u := Upstreams{Labels: map[string][]string{
			"com.docker.compose.service": {"first"},
		}}
		got, err := u.GetUpstreams(req)
		require.NoError(t, err)
		assert.ElementsMatch(t, []*reverseproxy.Upstream{first}, got)
	})

	t.Run("value list is ORed", func(t *testing.T) {
		u := Upstreams{Labels: map[string][]string{
			"com.docker.compose.service": {"first", "second"},
		}}
		got, err := u.GetUpstreams(req)
		require.NoError(t, err)
		assert.ElementsMatch(t, []*reverseproxy.Upstream{first, second}, got)
	})

	t.Run("keys are ANDed", func(t *testing.T) {
		u := Upstreams{Labels: map[string][]string{
			"com.docker.compose.service": {"first"},
			"missing.label":              {"whatever"},
		}}
		got, err := u.GetUpstreams(req)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
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

func candidateCount() int {
	candidatesMu.RLock()
	defer candidatesMu.RUnlock()
	return len(candidates)
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
	withCandidates(t)
	ctx, _ := newCancelableContext(t)

	stream := newEventStream()
	cli := &mockDockerClient{}
	cli.On("Events", mock.Anything, mock.Anything).Return(stream.result())
	cli.On("ContainerList", mock.Anything, mock.Anything).Return(oneContainerResult(), nil)
	cli.On("Close").Return(nil)

	u := &Upstreams{debounceInterval: time.Millisecond, reconnectDelay: time.Millisecond}
	done := make(chan struct{})
	go func() {
		u.keepUpdated(ctx, cli)
		close(done)
	}()

	// A container event must trigger a re-provision.
	stream.messages <- events.Message{}
	require.Eventually(t, func() bool { return candidateCount() == 1 }, 2*time.Second, time.Millisecond)

	// A canceled error stops the loop.
	stream.errs <- context.Canceled
	awaitReturn(t, done)

	cli.AssertCalled(t, "Close")
	cli.AssertExpectations(t)
}

func TestKeepUpdatedStopsOnCanceled(t *testing.T) {
	withCandidates(t)
	ctx, _ := newCancelableContext(t)

	stream := newEventStream()
	cli := &mockDockerClient{}
	cli.On("Events", mock.Anything, mock.Anything).Return(stream.result())
	cli.On("Close").Return(nil)

	u := &Upstreams{debounceInterval: time.Millisecond, reconnectDelay: time.Millisecond}
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
	withCandidates(t)
	ctx, _ := newCancelableContext(t)

	first := newEventStream()
	second := newEventStream()
	cli := &mockDockerClient{}
	// First connection, then a fresh connection after the reconnect delay.
	cli.On("Events", mock.Anything, mock.Anything).Return(first.result()).Once()
	cli.On("Events", mock.Anything, mock.Anything).Return(second.result()).Once()
	cli.On("Close").Return(nil)

	u := &Upstreams{debounceInterval: time.Millisecond, reconnectDelay: time.Millisecond}
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
	withCandidates(t)
	ctx, cancel := newCancelableContext(t)

	stream := newEventStream()
	cli := &mockDockerClient{}
	cli.On("Events", mock.Anything, mock.Anything).Return(stream.result())
	cli.On("Close").Return(nil)

	// A long reconnect delay ensures the context-cancel branch of the outer
	// select wins the race rather than a reconnect.
	u := &Upstreams{debounceInterval: time.Millisecond, reconnectDelay: time.Second}
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

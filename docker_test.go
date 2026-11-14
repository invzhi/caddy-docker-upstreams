package caddy_docker_upstreams

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// withCandidates saves and restores the package-level candidates so each test
// starts from a known state and does not leak into others.
func withCandidates(t *testing.T) {
	t.Helper()
	candidatesMu.Lock()
	prev := candidates
	candidates = nil
	candidatesMu.Unlock()
	t.Cleanup(func() {
		candidatesMu.Lock()
		candidates = prev
		candidatesMu.Unlock()
	})
}

// summary builds a minimal container summary for the tests.
func summary(id string, labels map[string]string, networks map[string]string) container.Summary {
	nets := make(map[string]*network.EndpointSettings, len(networks))
	for name, ip := range networks {
		nets[name] = &network.EndpointSettings{IPAddress: netip.MustParseAddr(ip)}
	}
	return container.Summary{
		ID:              id,
		Labels:          labels,
		NetworkSettings: &container.NetworkSettingsSummary{Networks: nets},
	}
}

// hostNetworkSummary builds a summary for a container using host networking,
// which Docker reports with a "host" endpoint carrying no IP address.
func hostNetworkSummary(id string, labels map[string]string) container.Summary {
	c := container.Summary{
		ID:     id,
		Labels: labels,
		NetworkSettings: &container.NetworkSettingsSummary{
			Networks: map[string]*network.EndpointSettings{"host": {}},
		},
	}
	c.HostConfig.NetworkMode = "host"
	return c
}

// dials renders each candidate as the address it would be dialed at when its
// port comes from the label; a candidate without a port label shows just the
// IP, since its effective port is supplied per block in GetUpstreams.
func dials(cs []candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.address
		if c.port != "" {
			out[i] = net.JoinHostPort(c.address, c.port)
		}
	}
	return out
}

func TestProvisionCandidates(t *testing.T) {
	tests := []struct {
		name       string
		containers []container.Summary
		wantDials  []string
	}{
		{
			name: "single container, implicit first network",
			containers: []container.Summary{
				summary("a",
					map[string]string{LabelUpstreamPort: "8080"},
					map[string]string{"bridge": "10.0.0.1"},
				),
			},
			wantDials: []string{"10.0.0.1:8080"},
		},
		{
			name: "explicit network is selected",
			containers: []container.Summary{
				summary("a",
					map[string]string{LabelUpstreamPort: "8080", LabelNetwork: "backend"},
					map[string]string{"bridge": "10.0.0.1", "backend": "172.20.0.5"},
				),
			},
			wantDials: []string{"172.20.0.5:8080"},
		},
		{
			name: "explicit network resolved via compose project prefix",
			containers: []container.Summary{
				summary("a",
					map[string]string{
						LabelUpstreamPort:            "9000",
						LabelNetwork:                 "backend",
						"com.docker.compose.project": "myproj",
					},
					map[string]string{"myproj_backend": "172.21.0.9"},
				),
			},
			wantDials: []string{"172.21.0.9:9000"},
		},
		{
			name: "host network mode dials the bare port",
			containers: []container.Summary{
				hostNetworkSummary("a", map[string]string{LabelUpstreamPort: "8080"}),
			},
			wantDials: []string{":8080"},
		},
		{
			name: "container without port label is kept without a port",
			containers: []container.Summary{
				summary("a",
					map[string]string{},
					map[string]string{"bridge": "10.0.0.1"},
				),
			},
			// The port is resolved per block in GetUpstreams, so the candidate
			// is kept here even though it carries no port label.
			wantDials: []string{"10.0.0.1"},
		},
		{
			name: "container without networks is skipped",
			containers: []container.Summary{
				summary("a",
					map[string]string{LabelUpstreamPort: "8080"},
					map[string]string{},
				),
			},
			wantDials: []string{},
		},
		{
			name: "explicit network missing and no project is skipped",
			containers: []container.Summary{
				summary("a",
					map[string]string{LabelUpstreamPort: "8080", LabelNetwork: "backend"},
					map[string]string{"bridge": "10.0.0.1"},
				),
			},
			wantDials: []string{},
		},
		{
			name: "explicit network missing even with wrong project is skipped",
			containers: []container.Summary{
				summary("a",
					map[string]string{
						LabelUpstreamPort:            "8080",
						LabelNetwork:                 "backend",
						"com.docker.compose.project": "myproj",
					},
					map[string]string{"bridge": "10.0.0.1"},
				),
			},
			wantDials: []string{},
		},
		{
			name: "containers with and without a port label are both kept",
			containers: []container.Summary{
				summary("labelled",
					map[string]string{LabelUpstreamPort: "8080"},
					map[string]string{"bridge": "10.0.0.1"},
				),
				summary("noport",
					map[string]string{},
					map[string]string{"bridge": "10.0.0.2"},
				),
			},
			wantDials: []string{"10.0.0.1:8080", "10.0.0.2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withCandidates(t)
			ctx := newTestContext(t)

			docker := &mockDockerClient{}
			docker.On("ContainerList", mock.Anything, mock.Anything).
				Return(client.ContainerListResult{Items: tt.containers}, nil)

			err := newTestDocker(ctx, docker).refreshCandidates()
			require.NoError(t, err)

			candidatesMu.RLock()
			got := dials(candidates)
			candidatesMu.RUnlock()

			assert.ElementsMatch(t, tt.wantDials, got)
			docker.AssertExpectations(t)
		})
	}
}

func TestProvisionCandidatesBuildsMatchers(t *testing.T) {
	withCandidates(t)
	ctx := newTestContext(t)

	docker := &mockDockerClient{}
	docker.On("ContainerList", mock.Anything, mock.Anything).
		Return(client.ContainerListResult{Items: []container.Summary{
			summary("a",
				map[string]string{
					LabelUpstreamPort: "8080",
					LabelMatchHost:    "example.com",
				},
				map[string]string{"bridge": "10.0.0.1"},
			),
		}}, nil)

	require.NoError(t, newTestDocker(ctx, docker).refreshCandidates())

	candidatesMu.RLock()
	defer candidatesMu.RUnlock()
	require.Len(t, candidates, 1)
	// The host matcher label must have produced exactly one matcher.
	assert.Len(t, candidates[0].matchers, 1)

	ok, err := candidates[0].matchers.MatchWithError(newRequest(t, "GET", "http://example.com/"))
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = candidates[0].matchers.MatchWithError(newRequest(t, "GET", "http://other.com/"))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestProvisionCandidatesListError(t *testing.T) {
	withCandidates(t)
	ctx := newTestContext(t)

	sentinel := errors.New("boom")
	docker := &mockDockerClient{}
	docker.On("ContainerList", mock.Anything, mock.Anything).
		Return(client.ContainerListResult{}, sentinel)

	err := newTestDocker(ctx, docker).refreshCandidates()
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	docker.AssertExpectations(t)
}

func newTestDocker(ctx context.Context, docker docker) *Docker {
	return &Docker{ctx: ctx, logger: zap.NewNop(), docker: docker}
}

// mockDockerClient is a testify mock implementing docker, shared by the
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
	docker := &mockDockerClient{}
	docker.On("Events", mock.Anything, mock.Anything).Return(stream.result())
	docker.On("ContainerList", mock.Anything, mock.Anything).Return(oneContainerResult(), nil)

	done := make(chan struct{})
	go func() {
		newTestDocker(ctx, docker).keepUpdated()
		close(done)
	}()

	// A container event must trigger a re-provision.
	stream.messages <- events.Message{}
	require.Eventually(t, func() bool { return candidateCount() == 1 }, 2*time.Second, time.Millisecond)

	// A canceled error stops the loop.
	stream.errs <- context.Canceled
	awaitReturn(t, done)

	// The shared client pool owns the client and closes it on final release.
	docker.AssertNotCalled(t, "Close")
	docker.AssertExpectations(t)
}

func TestKeepUpdatedStopsOnCanceled(t *testing.T) {
	withCandidates(t)
	ctx, _ := newCancelableContext(t)

	stream := newEventStream()
	docker := &mockDockerClient{}
	docker.On("Events", mock.Anything, mock.Anything).Return(stream.result())

	done := make(chan struct{})
	go func() {
		newTestDocker(ctx, docker).keepUpdated()
		close(done)
	}()

	stream.errs <- context.Canceled
	awaitReturn(t, done)

	// No event was delivered, so the container list is never fetched.
	docker.AssertNotCalled(t, "ContainerList", mock.Anything, mock.Anything)
	docker.AssertExpectations(t)
}

func TestKeepUpdatedReconnectsAfterError(t *testing.T) {
	withCandidates(t)
	ctx, _ := newCancelableContext(t)

	first := newEventStream()
	second := newEventStream()
	docker := &mockDockerClient{}
	// First connection, then a fresh connection after the reconnect delay.
	docker.On("Events", mock.Anything, mock.Anything).Return(first.result()).Once()
	docker.On("ContainerList", mock.Anything, mock.Anything).
		Return(oneContainerResult(), nil)
	docker.On("Events", mock.Anything, mock.Anything).Return(second.result()).Once()

	done := make(chan struct{})
	go func() {
		newTestDocker(ctx, docker).keepUpdated()
		close(done)
	}()

	// A transient (non-canceled) error breaks the inner loop and triggers a
	// reconnect: Events is invoked a second time.
	first.errs <- errors.New("boom")
	require.Eventually(t, func() bool {
		return docker.eventsCalls.Load() == 2
	}, 2*time.Second, time.Millisecond)

	// Stop via the second connection.
	second.errs <- context.Canceled
	awaitReturn(t, done)

	docker.AssertExpectations(t)
}

func TestKeepUpdatedReturnsOnContextCancelDuringBackoff(t *testing.T) {
	withCandidates(t)
	ctx, cancel := newCancelableContext(t)

	stream := newEventStream()
	docker := &mockDockerClient{}
	docker.On("Events", mock.Anything, mock.Anything).Return(stream.result())

	done := make(chan struct{})
	go func() {
		newTestDocker(ctx, docker).keepUpdated()
		close(done)
	}()

	// Break the inner loop with a transient error, then cancel while the loop
	// waits out the reconnect delay.
	stream.errs <- errors.New("boom")
	require.Eventually(t, func() bool {
		return docker.eventsCalls.Load() == 1
	}, 2*time.Second, time.Millisecond)
	cancel()

	awaitReturn(t, done)

	// It must have returned via ctx.Done(), without reconnecting.
	docker.AssertNumberOfCalls(t, "Events", 1)
	docker.AssertExpectations(t)
}

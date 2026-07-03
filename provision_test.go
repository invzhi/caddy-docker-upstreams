package caddy_docker_upstreams

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
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

func dials(cs []candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.upstream.Dial
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
			name: "container without port label is skipped",
			containers: []container.Summary{
				summary("a",
					map[string]string{},
					map[string]string{"bridge": "10.0.0.1"},
				),
			},
			wantDials: []string{},
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
			name: "valid and invalid containers are filtered",
			containers: []container.Summary{
				summary("ok",
					map[string]string{LabelUpstreamPort: "8080"},
					map[string]string{"bridge": "10.0.0.1"},
				),
				summary("noport",
					map[string]string{},
					map[string]string{"bridge": "10.0.0.2"},
				),
			},
			wantDials: []string{"10.0.0.1:8080"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withCandidates(t)
			ctx := newTestContext(t)

			cli := &mockDockerClient{}
			cli.On("ContainerList", mock.Anything, mock.Anything).
				Return(client.ContainerListResult{Items: tt.containers}, nil)

			var u Upstreams
			err := u.provisionCandidates(ctx, cli)
			require.NoError(t, err)

			candidatesMu.RLock()
			got := dials(candidates)
			candidatesMu.RUnlock()

			assert.ElementsMatch(t, tt.wantDials, got)
			cli.AssertExpectations(t)
		})
	}
}

func TestProvisionCandidatesBuildsMatchers(t *testing.T) {
	withCandidates(t)
	ctx := newTestContext(t)

	cli := &mockDockerClient{}
	cli.On("ContainerList", mock.Anything, mock.Anything).
		Return(client.ContainerListResult{Items: []container.Summary{
			summary("a",
				map[string]string{
					LabelUpstreamPort: "8080",
					LabelMatchHost:    "example.com",
				},
				map[string]string{"bridge": "10.0.0.1"},
			),
		}}, nil)

	var u Upstreams
	require.NoError(t, u.provisionCandidates(ctx, cli))

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
	cli := &mockDockerClient{}
	cli.On("ContainerList", mock.Anything, mock.Anything).
		Return(client.ContainerListResult{}, sentinel)

	var u Upstreams
	err := u.provisionCandidates(ctx, cli)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	cli.AssertExpectations(t)
}

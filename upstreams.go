package caddy_docker_upstreams

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
	"go.uber.org/zap"
)

const (
	LabelEnable       = "com.caddyserver.http.enable"
	LabelNetwork      = "com.caddyserver.http.network"
	LabelUpstreamPort = "com.caddyserver.http.upstream.port"
)

const (
	defaultDebounceInterval = 100 * time.Millisecond
	defaultReconnectDelay   = 500 * time.Millisecond
)

func init() {
	caddy.RegisterModule(Upstreams{})
}

// dockerClient is the subset of *client.Client used by this module.
// Its purpose is to allow testing this module with mocks.
type dockerClient interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	Events(ctx context.Context, options client.EventsListOptions) client.EventsResult
	Close() error
}

type candidate struct {
	matchers caddyhttp.MatcherSet
	labels   map[string]string
	upstream *reverseproxy.Upstream
}

var (
	candidates   []candidate
	candidatesMu sync.RWMutex
)

var defaultFilters = client.Filters{}.
	Add("label", fmt.Sprintf("%s=true", LabelEnable)).
	Add("status", "running"). // container.State.Status
	Add("health", string(container.Healthy), string(container.NoHealthcheck))

// Upstreams provides upstreams from the docker host.
type Upstreams struct {
	// Labels narrows the containers this source considers to those whose
	// Docker labels match. A container is selected only if, for every key,
	// its label value equals one of the listed values (keys are ANDed,
	// values within a key are ORed). An empty selector matches every
	// enabled container.
	//
	// This selects on container metadata rather than the request, so it can
	// distinguish otherwise-identical upstreams — e.g. pin routing to a
	// single Compose service via the label it already carries:
	//
	//	label com.docker.compose.service first
	Labels map[string][]string `json:"labels,omitempty"`

	// Port overrides the upstream port for every container this source
	// considers. When set, it takes precedence over the per-container
	// com.caddyserver.http.upstream.port label and makes that label optional.
	Port string `json:"port,omitempty"`

	debounceInterval time.Duration
	reconnectDelay   time.Duration
}

func (Upstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID: "http.reverse_proxy.upstreams.docker",
		New: func() caddy.Module {
			return &Upstreams{
				debounceInterval: defaultDebounceInterval,
				reconnectDelay:   defaultReconnectDelay,
			}
		},
	}
}

func (u *Upstreams) provisionCandidates(ctx caddy.Context, cli dockerClient) error {
	containers, err := cli.ContainerList(ctx, client.ContainerListOptions{Filters: defaultFilters})
	if err != nil {
		return fmt.Errorf("listing docker containers: %w", err)
	}

	updated := make([]candidate, 0, len(containers.Items))

	for _, c := range containers.Items {
		// Build matchers.
		matchers := buildMatchers(ctx, c.Labels)

		// Build upstream. A port configured in the Caddyfile takes precedence
		// over the per-container label and makes that label optional.
		port := u.Port
		if port == "" {
			var ok bool
			port, ok = c.Labels[LabelUpstreamPort]
			if !ok {
				ctx.Logger().Error("unable to get port from container labels or configuration",
					zap.String("container_id", c.ID),
				)
				continue
			}
		}

		// Choose network to connect.
		if len(c.NetworkSettings.Networks) == 0 {
			ctx.Logger().Error("unable to get ip address from container networks",
				zap.String("container_id", c.ID),
			)
			continue
		}

		network, ok := c.Labels[LabelNetwork]
		if !ok {
			// Use the first network settings of container.
			for _, settings := range c.NetworkSettings.Networks {
				address := net.JoinHostPort(settings.IPAddress.String(), port)
				updated = append(updated, candidate{
					matchers: matchers,
					labels:   c.Labels,
					upstream: &reverseproxy.Upstream{Dial: address},
				})
				break
			}
			continue
		}

		settings, ok := c.NetworkSettings.Networks[network]
		if !ok {
			// Add project prefix. See also https://github.com/compose-spec/compose-go/blob/main/loader/normalize.go.
			const projectLabel = "com.docker.compose.project"
			project, ok := c.Labels[projectLabel]
			if !ok {
				ctx.Logger().Error("unable to get network settings from container",
					zap.String("container_id", c.ID),
					zap.String("network", network),
				)
				continue
			}

			network = fmt.Sprintf("%s_%s", project, network)
			settings, ok = c.NetworkSettings.Networks[network]
			if !ok {
				ctx.Logger().Error("unable to get network settings from container",
					zap.String("container_id", c.ID),
					zap.String("network", network),
				)
				continue
			}
		}

		address := net.JoinHostPort(settings.IPAddress.String(), port)
		updated = append(updated, candidate{
			matchers: matchers,
			labels:   c.Labels,
			upstream: &reverseproxy.Upstream{Dial: address},
		})
	}

	candidatesMu.Lock()
	candidates = updated
	candidatesMu.Unlock()

	return nil
}

func (u *Upstreams) keepUpdated(ctx caddy.Context, cli dockerClient) {
	defer cli.Close()

	debounced := debounce.New(u.debounceInterval)

	for {
		messages := cli.Events(ctx, client.EventsListOptions{
			Filters: client.Filters{}.Add("type", string(events.ContainerEventType)),
		})

	selectLoop:
		for {
			select {
			case <-messages.Messages:
				debounced(func() {
					err := u.provisionCandidates(ctx, cli)
					if err != nil {
						ctx.Logger().Error("unable to provision the candidates", zap.Error(err))
					}
				})
			case err := <-messages.Err:
				if errors.Is(err, context.Canceled) {
					return
				}

				ctx.Logger().Warn("unable to monitor container events; will retry", zap.Error(err))
				break selectLoop
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(u.reconnectDelay):
		}
	}
}

func (u *Upstreams) provision(ctx caddy.Context, cli *client.Client) error {
	err := u.provisionCandidates(ctx, cli)
	if err != nil {
		return err
	}

	go u.keepUpdated(ctx, cli)

	return nil
}

func (u *Upstreams) Provision(ctx caddy.Context) error {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return fmt.Errorf("provisioning docker client: %w", err)
	}

	ping, err := cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
	if err != nil {
		return fmt.Errorf("ping docker server: %w", err)
	}
	ctx.Logger().Info("connected docker server", zap.String("api_version", ping.APIVersion))

	return u.provision(ctx, cli)
}

func (u *Upstreams) GetUpstreams(r *http.Request) ([]*reverseproxy.Upstream, error) {
	upstreams := make([]*reverseproxy.Upstream, 0, 1)

	candidatesMu.RLock()
	defer candidatesMu.RUnlock()

	for _, c := range candidates {
		if !u.selects(c) {
			continue
		}
		if c.matchers.Match(r) {
			upstreams = append(upstreams, c.upstream)
		}
	}

	return upstreams, nil
}

// selects reports whether the candidate's container satisfies u.Labels. Every
// configured key must be present with a value among those listed for it.
func (u *Upstreams) selects(c candidate) bool {
	for key, values := range u.Labels {
		got, ok := c.labels[key]
		if !ok || !slices.Contains(values, got) {
			return false
		}
	}
	return true
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Upstreams)(nil)
	_ reverseproxy.UpstreamSource = (*Upstreams)(nil)
)

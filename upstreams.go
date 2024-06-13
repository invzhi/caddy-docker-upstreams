package caddy_docker_upstreams

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"go.uber.org/zap"
)

const (
	LabelEnable       = "com.caddyserver.http.enable"
	LabelNetwork      = "com.caddyserver.http.network"
	LabelUpstreamPort = "com.caddyserver.http.upstream.port"
)

func init() {
	caddy.RegisterModule(Upstreams{})
}

type candidate struct {
	matchers caddyhttp.MatcherSet
	upstream *reverseproxy.Upstream
}

var (
	candidates   []candidate
	candidatesMu sync.RWMutex
)

var defaultFilters = filters.NewArgs(
	filters.Arg("label", fmt.Sprintf("%s=true", LabelEnable)),
	filters.Arg("status", "running"), // types.ContainerState.Status
	filters.Arg("health", types.Healthy),
	filters.Arg("health", types.NoHealthcheck),
)

// Upstreams provides upstreams from the docker host.
type Upstreams struct {
}

func (Upstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.docker",
		New: func() caddy.Module { return new(Upstreams) },
	}
}

func (u *Upstreams) provisionCandidates(ctx caddy.Context, containers []types.Container) {
	updated := make([]candidate, 0, len(containers))

	for _, c := range containers {
		// Build matchers.
		matchers := buildMatchers(ctx, c.Labels)

		// Build upstream.
		port, ok := c.Labels[LabelUpstreamPort]
		if !ok {
			ctx.Logger().Error("unable to get port from container labels",
				zap.String("container_id", c.ID),
			)
			continue
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
				address := net.JoinHostPort(settings.IPAddress, port)
				updated = append(updated, candidate{
					matchers: matchers,
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

		address := net.JoinHostPort(settings.IPAddress, port)
		updated = append(updated, candidate{
			matchers: matchers,
			upstream: &reverseproxy.Upstream{Dial: address},
		})
	}

	candidatesMu.Lock()
	candidates = updated
	candidatesMu.Unlock()
}

func (u *Upstreams) keepUpdated(ctx caddy.Context, cli *client.Client) {
	debounced := debounce.New(100 * time.Millisecond)

	for {
		messages, errs := cli.Events(ctx, types.EventsOptions{
			Filters: filters.NewArgs(filters.Arg("type", string(events.ContainerEventType))),
		})

	selectLoop:
		for {
			select {
			case <-messages:
				debounced(func() {
					containers, err := cli.ContainerList(ctx, container.ListOptions{Filters: defaultFilters})
					if err != nil {
						ctx.Logger().Error("unable to get the list of containers", zap.Error(err))
						return
					}

					u.provisionCandidates(ctx, containers)
				})
			case err := <-errs:
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
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (u *Upstreams) Provision(ctx caddy.Context) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	ctx.OnCancel(func() { _ = cli.Close() })

	ping, err := cli.Ping(ctx)
	if err != nil {
		return err
	}

	ctx.Logger().Info("docker engine is connected", zap.String("api_version", ping.APIVersion))

	containers, err := cli.ContainerList(ctx, container.ListOptions{Filters: defaultFilters})
	if err != nil {
		return err
	}

	u.provisionCandidates(ctx, containers)

	go u.keepUpdated(ctx, cli)

	return nil
}

func (u *Upstreams) GetUpstreams(r *http.Request) ([]*reverseproxy.Upstream, error) {
	upstreams := make([]*reverseproxy.Upstream, 0, 1)

	candidatesMu.RLock()
	defer candidatesMu.RUnlock()

	for _, c := range candidates {
		if c.matchers.Match(r) {
			upstreams = append(upstreams, c.upstream)
		}
	}

	return upstreams, nil
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Upstreams)(nil)
	_ reverseproxy.UpstreamSource = (*Upstreams)(nil)
)

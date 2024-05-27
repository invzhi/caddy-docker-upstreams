package caddy_docker_upstreams

import (
	"context"
	"errors"
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

// Upstreams provides upstreams from the docker host.
type Upstreams struct {
	logger *zap.Logger
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
		// Check enable.
		if enable, ok := c.Labels[LabelEnable]; !ok || enable != "true" {
			continue
		}

		// Build matchers.
		matchers := buildMatchers(ctx, u.logger, c.Labels)

		// Build upstream.
		port, ok := c.Labels[LabelUpstreamPort]
		if !ok {
			u.logger.Error("unable to get port from container labels",
				zap.String("container_id", c.ID),
			)
			continue
		}

		// Choose network to connect.
		if len(c.NetworkSettings.Networks) == 0 {
			u.logger.Error("unable to get ip address from container networks",
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
			u.logger.Error("unable to get network settings from container",
				zap.String("container_id", c.ID),
				zap.String("network", network),
			)
			continue
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
					containers, err := cli.ContainerList(ctx, container.ListOptions{
						Filters: filters.NewArgs(filters.Arg("label", LabelEnable)),
					})
					if err != nil {
						u.logger.Error("unable to get the list of containers", zap.Error(err))
						return
					}

					u.provisionCandidates(ctx, containers)
				})
			case err := <-errs:
				if errors.Is(err, context.Canceled) {
					return
				}

				u.logger.Warn("unable to monitor container events; will retry", zap.Error(err))
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
	u.logger = ctx.Logger()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}

	ping, err := cli.Ping(ctx)
	if err != nil {
		return err
	}

	u.logger.Info("docker engine is connected", zap.String("api_version", ping.APIVersion))

	containers, err := cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", LabelEnable)),
	})
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

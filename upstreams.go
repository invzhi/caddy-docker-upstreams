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
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"go.uber.org/zap"
)

const (
	LabelEnable       = "com.caddyserver.http.enable"
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
	filters.Arg("label", LabelEnable),
	// types.ContainerState.Status
	filters.Arg("status", "running"),
	filters.Arg("health", types.Healthy),
	filters.Arg("health", types.NoHealthcheck),
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

	for _, container := range containers {
		// Check enable.
		if enable, ok := container.Labels[LabelEnable]; !ok || enable != "true" {
			continue
		}

		// Build matchers.
		var matchers caddyhttp.MatcherSet

		for key, producer := range producers {
			value, ok := container.Labels[key]
			if !ok {
				continue
			}

			matcher, err := producer(value)
			if err != nil {
				u.logger.Error("unable to load matcher",
					zap.String("key", key),
					zap.String("value", value),
					zap.Error(err),
				)
				continue
			}

			if prov, ok := matcher.(caddy.Provisioner); ok {
				err = prov.Provision(ctx)
				if err != nil {
					u.logger.Error("unable to provision matcher",
						zap.String("key", key),
						zap.String("value", value),
						zap.Error(err),
					)
					continue
				}
			}

			matchers = append(matchers, matcher)
		}

		// Build upstream.
		port, ok := container.Labels[LabelUpstreamPort]
		if !ok {
			u.logger.Error("unable to get port from container labels",
				zap.String("container_id", container.ID),
			)
			continue
		}

		if len(container.NetworkSettings.Networks) == 0 {
			u.logger.Error("unable to get ip address from container networks",
				zap.String("container_id", container.ID),
			)
			continue
		}

		// Use the first network settings of container.
		for _, settings := range container.NetworkSettings.Networks {
			address := net.JoinHostPort(settings.IPAddress, port)
			upstream := &reverseproxy.Upstream{Dial: address}

			updated = append(updated, candidate{
				matchers: matchers,
				upstream: upstream,
			})
			break
		}
	}

	candidatesMu.Lock()
	candidates = updated
	candidatesMu.Unlock()
}

func (u *Upstreams) keepUpdated(ctx caddy.Context, cli *client.Client) {
	debounced := debounce.New(100 * time.Millisecond)

	for {
		messages, errs := cli.Events(ctx, types.EventsOptions{
			Filters: filters.NewArgs(filters.Arg("type", events.ContainerEventType)),
		})

	selectLoop:
		for {
			select {
			case <-messages:
				debounced(func() {
					containers, err := cli.ContainerList(ctx, types.ContainerListOptions{
						Filters: defaultFilters,
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

	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{
		Filters: defaultFilters,
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

	for _, container := range candidates {
		if !container.matchers.Match(r) {
			continue
		}

		upstreams = append(upstreams, container.upstream)
	}

	return upstreams, nil
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Upstreams)(nil)
	_ reverseproxy.UpstreamSource = (*Upstreams)(nil)
)

package caddy_docker_upstreams

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
	"go.uber.org/zap"
)

const (
	defaultDebounceDuration  = 100 * time.Millisecond
	defaultReconnectDuraiton = 500 * time.Millisecond
)

const (
	LabelEnable       = "com.caddyserver.http.enable"
	LabelNetwork      = "com.caddyserver.http.network"
	LabelUpstreamPort = "com.caddyserver.http.upstream.port"
)

var defaultFilters = client.Filters{}.
	Add("label", fmt.Sprintf("%s=true", LabelEnable)).
	Add("status", "running") // container.State.Status

var (
	candidates   []candidate
	candidatesMu sync.RWMutex
)

type candidate struct {
	matchers caddyhttp.MatcherSet
	labels   map[string]string
	address  string // container IP address, without a port
	port     string // port from the upstream.port label; empty when the label is absent
}

// docker is the subset of *client.Client used by this module.
// Its purpose is to allow testing this module with mocks.
type docker interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	Events(ctx context.Context, options client.EventsListOptions) client.EventsResult
	Close() error
}

type Docker struct {
	ctx    context.Context
	cancel context.CancelFunc
	logger *zap.Logger
	docker
}

func NewDocker(logger *zap.Logger) (*Docker, error) {
	docker, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("provisioning docker client: %w", err)
	}

	ctx := context.Background()
	ping, err := docker.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
	if err != nil {
		return nil, fmt.Errorf("ping docker server: %w", err)
	}

	logger.Info("connected docker server", zap.String("api_version", ping.APIVersion))

	ctx, cancel := context.WithCancel(ctx)
	d := &Docker{ctx: ctx, cancel: cancel, logger: logger, docker: docker}
	if err := d.refreshCandidates(); err != nil {
		return nil, err
	}
	go d.keepUpdated()

	return d, nil
}

func (d *Docker) containerHealthStatus(c container.Summary) (container.HealthStatus, error) {
	if c.Health != nil {
		return c.Health.Status, nil
	}

	// Docker API 1.52 added Health to container list responses. For older APIs
	// and compatible servers that omit it, inspect the container instead.
	result, err := d.ContainerInspect(d.ctx, c.ID, client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspecting container: %w", err)
	}

	if result.Container.State == nil {
		return "", errors.New("container state is null")
	}

	// Inspect State.Health is optional: nil means no healthcheck is configured,
	// including on current Docker API versions and compatible servers.
	if result.Container.State.Health == nil {
		return container.NoHealthcheck, nil
	}

	return result.Container.State.Health.Status, nil
}

func (d *Docker) refreshCandidates() error {
	containers, err := d.ContainerList(d.ctx, client.ContainerListOptions{Filters: defaultFilters})
	if err != nil {
		return fmt.Errorf("listing docker containers: %w", err)
	}

	updated := make([]candidate, 0, len(containers.Items))

	for _, c := range containers.Items {
		healthStatus, err := d.containerHealthStatus(c)
		if err != nil {
			d.logger.Error("unable to get container health state",
				zap.String("container_id", c.ID),
				zap.Error(err),
			)
			continue
		}
		if healthStatus != container.NoHealthcheck && healthStatus != container.Healthy {
			continue
		}

		// Build matchers.
		matchers := buildMatchers(d.ctx, d.logger, c.Labels)

		// Candidates are shared by every dynamic docker block, so provisioning
		// must not fold in per-block configuration such as the port directive.
		// Record the container IP and its optional port label here; the
		// effective port is resolved per request in GetUpstreams.

		// Containers sharing the host's network namespace have no address of
		// their own; dialing the bare port reaches that namespace.
		if c.HostConfig.NetworkMode == "host" {
			updated = append(updated, candidate{
				matchers: matchers,
				labels:   c.Labels,
				port:     c.Labels[LabelUpstreamPort],
			})
			continue
		}

		// Choose network to connect.
		if len(c.NetworkSettings.Networks) == 0 {
			d.logger.Error("unable to get ip address from container networks",
				zap.String("container_id", c.ID),
			)
			continue
		}

		var address string
		network, ok := c.Labels[LabelNetwork]
		if !ok {
			// Use the first network settings of container.
			for _, settings := range c.NetworkSettings.Networks {
				address = settings.IPAddress.String()
				break
			}
		} else {
			settings, ok := c.NetworkSettings.Networks[network]
			if !ok {
				// Add project prefix. See also https://github.com/compose-spec/compose-go/blob/main/loader/normalize.go.
				const projectLabel = "com.docker.compose.project"
				project, ok := c.Labels[projectLabel]
				if !ok {
					d.logger.Error("unable to get network settings from container",
						zap.String("container_id", c.ID),
						zap.String("network", network),
					)
					continue
				}

				network = fmt.Sprintf("%s_%s", project, network)
				settings, ok = c.NetworkSettings.Networks[network]
				if !ok {
					d.logger.Error("unable to get network settings from container",
						zap.String("container_id", c.ID),
						zap.String("network", network),
					)
					continue
				}
			}
			address = settings.IPAddress.String()
		}

		updated = append(updated, candidate{
			matchers: matchers,
			labels:   c.Labels,
			address:  address,
			port:     c.Labels[LabelUpstreamPort],
		})
	}

	candidatesMu.Lock()
	candidates = updated
	candidatesMu.Unlock()

	return nil
}

func (d *Docker) keepUpdated() {
	debounced := debounce.New(defaultDebounceDuration)

	for {
		messages := d.Events(d.ctx, client.EventsListOptions{
			Filters: client.Filters{}.Add("type", string(events.ContainerEventType)),
		})

	selectLoop:
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-messages.Messages:
				debounced(func() {
					err := d.refreshCandidates()
					if err != nil {
						d.logger.Error("unable to provision the candidates", zap.Error(err))
					}
				})
			case err := <-messages.Err:
				if errors.Is(err, context.Canceled) {
					return
				}

				d.logger.Warn("unable to monitor container events; will retry", zap.Error(err))
				break selectLoop
			}
		}

		select {
		case <-d.ctx.Done():
			return
		case <-time.After(defaultReconnectDuraiton):
			err := d.refreshCandidates()
			if err != nil {
				d.logger.Error("unable to provision the candidates", zap.Error(err))
			}
		}
	}
}

func (d *Docker) Destruct() error {
	d.cancel()
	return d.Close()
}

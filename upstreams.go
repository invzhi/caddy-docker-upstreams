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
	defaultDebounceDuration  = 100 * time.Millisecond
	defaultReconnectDuraiton = 500 * time.Millisecond
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
	address  string // container IP address, without a port
	port     string // port from the upstream.port label; empty when the label is absent
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
}

func (Upstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.docker",
		New: func() caddy.Module { return new(Upstreams) },
	}
}

func provisionCandidates(ctx context.Context, logger *zap.Logger, docker dockerClient) error {
	containers, err := docker.ContainerList(ctx, client.ContainerListOptions{Filters: defaultFilters})
	if err != nil {
		return fmt.Errorf("listing docker containers: %w", err)
	}

	updated := make([]candidate, 0, len(containers.Items))

	for _, c := range containers.Items {
		// Build matchers.
		matchers := buildMatchers(ctx, logger, c.Labels)

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
			logger.Error("unable to get ip address from container networks",
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
					logger.Error("unable to get network settings from container",
						zap.String("container_id", c.ID),
						zap.String("network", network),
					)
					continue
				}

				network = fmt.Sprintf("%s_%s", project, network)
				settings, ok = c.NetworkSettings.Networks[network]
				if !ok {
					logger.Error("unable to get network settings from container",
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

func keepUpdated(ctx context.Context, logger *zap.Logger, docker dockerClient) {
	debounced := debounce.New(defaultDebounceDuration)

	for {
		messages := docker.Events(ctx, client.EventsListOptions{
			Filters: client.Filters{}.Add("type", string(events.ContainerEventType)),
		})

	selectLoop:
		for {
			select {
			case <-ctx.Done():
				return
			case <-messages.Messages:
				debounced(func() {
					err := provisionCandidates(ctx, logger, docker)
					if err != nil {
						logger.Error("unable to provision the candidates", zap.Error(err))
					}
				})
			case err := <-messages.Err:
				if errors.Is(err, context.Canceled) {
					return
				}

				logger.Warn("unable to monitor container events; will retry", zap.Error(err))
				break selectLoop
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(defaultReconnectDuraiton):
		}
	}
}

type clientDestructor struct {
	docker *client.Client
	cancel context.CancelFunc
}

func (c clientDestructor) Destruct() error {
	c.cancel()
	return c.docker.Close()
}

const key = "docker"

var clients = caddy.NewUsagePool()

func (u *Upstreams) Provision(ctx caddy.Context) error {
	_, _, err := clients.LoadOrNew(key, func() (caddy.Destructor, error) {
		docker, err := client.New(client.FromEnv)
		if err != nil {
			return nil, fmt.Errorf("provisioning docker client: %w", err)
		}

		ping, err := docker.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
		if err != nil {
			return nil, fmt.Errorf("ping docker server: %w", err)
		}

		logger := ctx.Logger()
		logger.Info("connected docker server", zap.String("api_version", ping.APIVersion))

		err = provisionCandidates(ctx, logger, docker)
		if err != nil {
			return nil, err
		}

		// Reset the context so configuration reloads don't cancel the shared watcher.
		ctx, cancel := context.WithCancel(context.Background())
		go keepUpdated(ctx, logger, docker)

		return clientDestructor{docker: docker, cancel: cancel}, nil
	})
	return err
}

func (u *Upstreams) Cleanup() error {
	_, err := clients.Delete(key)
	return err
}

// selects reports whether the candidate's container satisfies u.Labels. Every
// configured key must be present with a value among those listed for it.
func (u *Upstreams) selects(c candidate) bool {
	for key, values := range u.Labels {
		value, ok := c.labels[key]
		if !ok || !slices.Contains(values, value) {
			return false
		}
	}
	return true
}

func (u *Upstreams) GetUpstreams(r *http.Request) ([]*reverseproxy.Upstream, error) {
	upstreams := make([]*reverseproxy.Upstream, 0, 1)

	candidatesMu.RLock()
	defer candidatesMu.RUnlock()

	for _, c := range candidates {
		if !u.selects(c) {
			continue
		}
		if !c.matchers.Match(r) {
			continue
		}

		// Resolve the port for this block: the port directive takes precedence
		// over the per-container label. This is done here rather than at
		// provision time because candidates are shared across all blocks.
		port := u.Port
		if port == "" {
			port = c.port
		}
		if port == "" {
			continue
		}

		upstreams = append(upstreams, &reverseproxy.Upstream{Dial: net.JoinHostPort(c.address, port)})
	}

	return upstreams, nil
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Upstreams)(nil)
	_ caddy.CleanerUpper          = (*Upstreams)(nil)
	_ reverseproxy.UpstreamSource = (*Upstreams)(nil)
)

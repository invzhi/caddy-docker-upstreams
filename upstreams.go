package caddy_docker_upstreams

import (
	"net"
	"net/http"
	"slices"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

func init() {
	caddy.RegisterModule(Upstreams{})
}

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

const key = "docker"

var clients = caddy.NewUsagePool()

func (u *Upstreams) Provision(ctx caddy.Context) error {
	_, _, err := clients.LoadOrNew(key, func() (caddy.Destructor, error) {
		return NewDocker(ctx.Logger())
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
	upstreams := make([]*reverseproxy.Upstream, 0)

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

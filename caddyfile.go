package caddy_docker_upstreams

import "github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

// UnmarshalCaddyfile deserializes Caddyfile tokens into u.
//
//	dynamic docker
func (u *Upstreams) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		if d.NextArg() {
			return d.ArgErr()
		}
		if d.NextBlock(0) {
			return d.Errf("unrecognized docker option '%s'", d.Val())
		}
	}
	return nil
}

// Interface guards
var (
	_ caddyfile.Unmarshaler = (*Upstreams)(nil)
)

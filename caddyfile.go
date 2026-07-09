package caddy_docker_upstreams

import "github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

// UnmarshalCaddyfile deserializes Caddyfile tokens into u.
//
//	dynamic docker {
//	    label <key> <value...>
//	    port <port>
//	}
func (u *Upstreams) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		if d.NextArg() {
			return d.ArgErr()
		}
		for d.NextBlock(0) {
			switch d.Val() {
			case "label":
				args := d.RemainingArgs()
				if len(args) < 2 {
					return d.ArgErr()
				}
				if u.Labels == nil {
					u.Labels = make(map[string][]string)
				}
				key, values := args[0], args[1:]
				u.Labels[key] = append(u.Labels[key], values...)
			case "port":
				if !d.NextArg() {
					return d.ArgErr()
				}
				u.Port = d.Val()
				if d.NextArg() {
					return d.ArgErr()
				}
			default:
				return d.Errf("unrecognized docker option '%s'", d.Val())
			}
		}
	}
	return nil
}

// Interface guards
var (
	_ caddyfile.Unmarshaler = (*Upstreams)(nil)
)

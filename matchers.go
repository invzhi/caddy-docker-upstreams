package caddy_docker_upstreams

import "github.com/caddyserver/caddy/v2/modules/caddyhttp"

const (
	LabelMatchProtocol = "com.caddyserver.http.matchers.protocol"
	LabelMatchHost     = "com.caddyserver.http.matchers.host"
	LabelMatchPath     = "com.caddyserver.http.matchers.path"
)

var producers = map[string]func(string) caddyhttp.RequestMatcher{
	// TODO: more matchers
	LabelMatchProtocol: func(value string) caddyhttp.RequestMatcher {
		return caddyhttp.MatchProtocol(value)
	},
	LabelMatchHost: func(value string) caddyhttp.RequestMatcher {
		return caddyhttp.MatchHost([]string{value})
	},
	LabelMatchPath: func(value string) caddyhttp.RequestMatcher {
		return caddyhttp.MatchPath([]string{value})
	},
}

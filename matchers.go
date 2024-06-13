package caddy_docker_upstreams

import (
	"net/url"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

const (
	LabelMatchProtocol   = "com.caddyserver.http.matchers.protocol"
	LabelMatchHost       = "com.caddyserver.http.matchers.host"
	LabelMatchMethod     = "com.caddyserver.http.matchers.method"
	LabelMatchPath       = "com.caddyserver.http.matchers.path"
	LabelMatchQuery      = "com.caddyserver.http.matchers.query"
	LabelMatchExpression = "com.caddyserver.http.matchers.expression"
)

var producers = map[string]func(string) (caddyhttp.RequestMatcher, error){
	LabelMatchProtocol: func(value string) (caddyhttp.RequestMatcher, error) {
		return caddyhttp.MatchProtocol(value), nil
	},
	LabelMatchHost: func(value string) (caddyhttp.RequestMatcher, error) {
		return caddyhttp.MatchHost{value}, nil
	},
	LabelMatchMethod: func(value string) (caddyhttp.RequestMatcher, error) {
		return caddyhttp.MatchMethod{value}, nil
	},
	LabelMatchPath: func(value string) (caddyhttp.RequestMatcher, error) {
		return caddyhttp.MatchPath{value}, nil
	},
	LabelMatchQuery: func(value string) (caddyhttp.RequestMatcher, error) {
		query, err := url.ParseQuery(value)
		if err != nil {
			return nil, err
		}
		return caddyhttp.MatchQuery(query), nil
	},
	LabelMatchExpression: func(value string) (caddyhttp.RequestMatcher, error) {
		return caddyhttp.MatchExpression{Expr: value}, nil
	},
}

func buildMatchers(ctx caddy.Context, labels map[string]string) caddyhttp.MatcherSet {
	var matchers caddyhttp.MatcherSet

	for key, producer := range producers {
		value, ok := labels[key]
		if !ok {
			continue
		}

		matcher, err := producer(value)
		if err != nil {
			ctx.Logger().Error("unable to load matcher",
				zap.String("key", key),
				zap.String("value", value),
				zap.Error(err),
			)
			continue
		}

		if prov, ok := matcher.(caddy.Provisioner); ok {
			err = prov.Provision(ctx)
			if err != nil {
				ctx.Logger().Error("unable to provision matcher",
					zap.String("key", key),
					zap.String("value", value),
					zap.Error(err),
				)
				continue
			}
		}

		matchers = append(matchers, matcher)
	}

	return matchers
}

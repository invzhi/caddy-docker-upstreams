# Docker Dynamic Upstreams for Caddy.

This package implements a docker dynamic upstreams module for Caddy.

Requires Caddy 2+.

## Docker Labels

This module requires the Docker Labels to provide the necessary information.

- `com.caddyserver.http.enable` should be `true`
- `com.caddyserver.http.upstream.port` specify the port

As well as the labels corresponding to the matcher.

| Label                                      | Matcher                                                                  |
|--------------------------------------------|--------------------------------------------------------------------------|
| `com.caddyserver.http.matchers.protocol`   | [protocol](https://caddyserver.com/docs/caddyfile/matchers#protocol)     |
| `com.caddyserver.http.matchers.host`       | [host](https://caddyserver.com/docs/caddyfile/matchers#host)             |
| `com.caddyserver.http.matchers.method`     | [method](https://caddyserver.com/docs/caddyfile/matchers#method)         |
| `com.caddyserver.http.matchers.path`       | [path](https://caddyserver.com/docs/caddyfile/matchers#path)             |
| `com.caddyserver.http.matchers.query`      | [query](https://caddyserver.com/docs/caddyfile/matchers#query)           |
| `com.caddyserver.http.matchers.expression` | [expression](https://caddyserver.com/docs/caddyfile/matchers#expression) |

Here is an example with [vaultwarden](https://github.com/dani-garcia/vaultwarden).

```yaml
vaultwarden:
  image: vaultwarden/server:${VAULTWARDEN_VERSION:-latest}
  restart: unless-stopped
  volumes:
    - ${VAULTWARDEN_ROOT}:/data
  labels:
    com.caddyserver.http.enable: true
    com.caddyserver.http.upstream.port: 80
    com.caddyserver.http.matchers.host: vaultwarden.example.com
  environment:
    DOMAIN: https://vaultwarden.example.com
```

## Syntax

List all your domain or use [On-Demand TLS](https://caddyserver.com/docs/automatic-https#on-demand-tls).

```
app1.example.com,
app2.example.com,
app3.example.com {
    reverse_proxy {
        dynamic docker
    }
}
```

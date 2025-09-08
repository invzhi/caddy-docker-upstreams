# Docker Dynamic Upstreams for Caddy.

This package implements a docker dynamic upstreams module for Caddy.

Requires Caddy 2+.

## Installation

Download from [official website](https://caddyserver.com/download?package=github.com%2Finvzhi%2Fcaddy-docker-upstreams)
or build yourself using [xcaddy](https://github.com/caddyserver/xcaddy).

Here is a Dockerfile example.

```dockerfile
FROM caddy:<version>-builder AS builder

RUN xcaddy build \
    --with github.com/invzhi/caddy-docker-upstreams

FROM caddy:<version>

COPY --from=builder /usr/bin/caddy /usr/bin/caddy
```

## Caddyfile Syntax

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

## Docker Labels

This module requires the Docker Labels to provide the necessary information.

| Label                                | Description                                                                                                                            |
|--------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------|
| `com.caddyserver.http.enable`        | required, should be `true`                                                                                                             |
| `com.caddyserver.http.network`       | optional, specify the docker network which caddy connecting through (if it is empty, the first network of container will be specified) |
| `com.caddyserver.http.upstream.port` | required, specify the port                                                                                                             |

As well as the labels corresponding to the matcher.

| Label                                      | Matcher                                                                  | Type       |
|--------------------------------------------|--------------------------------------------------------------------------|------------|
| `com.caddyserver.http.matchers.protocol`   | [protocol](https://caddyserver.com/docs/caddyfile/matchers#protocol)     | `string`   |
| `com.caddyserver.http.matchers.host`       | [host](https://caddyserver.com/docs/caddyfile/matchers#host)             | `[]string` |
| `com.caddyserver.http.matchers.method`     | [method](https://caddyserver.com/docs/caddyfile/matchers#method)         | `[]string` |
| `com.caddyserver.http.matchers.path`       | [path](https://caddyserver.com/docs/caddyfile/matchers#path)             | `[]string` |
| `com.caddyserver.http.matchers.query`      | [query](https://caddyserver.com/docs/caddyfile/matchers#query)           | `string`   |
| `com.caddyserver.http.matchers.expression` | [expression](https://caddyserver.com/docs/caddyfile/matchers#expression) | `string`   |

Here is a docker-compose.yml example with [vaultwarden](https://github.com/dani-garcia/vaultwarden).

```yaml
vaultwarden:
  image: vaultwarden/server:${VAULTWARDEN_VERSION:-latest}
  restart: unless-stopped
  volumes:
    - ${VAULTWARDEN_ROOT}:/data
  labels:
    com.caddyserver.http.enable: true
    com.caddyserver.http.upstream.port: 80
    com.caddyserver.http.matchers.host: "vaultwarden.example.com bitwarden.example.com"
  environment:
    DOMAIN: https://vaultwarden.example.com
```

## Docker Client

Environment variables could configure the docker client:

- `DOCKER_HOST` to set the URL to the docker server.
- `DOCKER_API_VERSION` to set the version of the API to use, leave empty for latest.
- `DOCKER_CERT_PATH` to specify the directory from which to load the TLS certificates ("ca.pem", "cert.pem", "key.pem').
- `DOCKER_TLS_VERIFY` to enable or disable TLS verification (off by default).

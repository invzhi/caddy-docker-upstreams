# Smoke test

An end-to-end check of the plugin against a real Docker daemon, using a
deliberately varied Compose project to exercise upstream **port** selection
across several `dynamic docker` blocks at once:

| Endpoint                 | Service      | Expected body   | Port comes from            |
|--------------------------|--------------|-----------------|----------------------------|
| `http://localhost:9001/` | `alpha`      | `alpha`         | `port 5001` directive      |
| `http://localhost:9002/` | `beta`       | `beta`          | `port 5002` directive      |
| `http://localhost:9003/` | `multi`      | `multi api`     | `port 8080` directive      |
| `http://localhost:9004/` | `multi`      | `multi metrics` | `port 9090` directive      |
| `http://localhost:9005/` | `web` (×2)   | `web`           | `com.caddyserver.http.upstream.port` label |

- `alpha` / `beta` run the **same image** with different commands and ports.
- `multi` is **one container exposing two ports** (an "API" and a "metrics" port);
  two sites select it but expect different ports.
- `web` is **scaled to two replicas** and carries the port label, so its site
  needs no `port` directive.

This mix is what surfaced a port-selection bug: `alpha`, `beta` and `multi`
each need a different port supplied by their block's `port` directive, so it is
a direct check that per-block ports do not leak across blocks through the
shared candidate list.

## Run it

The whole flow — build, bring up, assert every route, tear down — is automated:

```sh
./run.sh
```

It exits non-zero if any route returns the wrong upstream.

To drive it by hand instead, everything runs from this directory.

1. Build a custom Caddy with this plugin from the local checkout:

   ```sh
   xcaddy build --with github.com/invzhi/caddy-docker-upstreams=.. --output ./caddy
   ```

2. Start the services:

   ```sh
   docker compose up -d
   ```

3. Run Caddy (needs access to the Docker socket; DEBUG logging is on so you can
   watch upstream selection):

   ```sh
   ./caddy run --config Caddyfile
   ```

4. In another shell, probe every endpoint — each must return its own body:

   ```sh
   curl http://localhost:9001/        # alpha
   curl http://localhost:9002/        # beta
   curl http://localhost:9003/        # multi api
   curl http://localhost:9003/other   # multi metrics
   curl http://localhost:9004/        # multi metrics
   curl http://localhost:9004/other   # multi api
   curl http://localhost:9005/        # web
   ```

5. Tear down:

   ```sh
   docker compose down
   ```

## Notes

- Caddy runs on the host and dials container IPs directly, which works out of
  the box on Linux. On Docker Desktop (macOS/Windows) the bridge network is not
  routable from the host — run Caddy as a container on the same Compose network
  instead.
- The `./caddy` binary is a build artifact and is git-ignored.

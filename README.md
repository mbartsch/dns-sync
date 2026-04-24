# dns-sync

Watches Docker container lifecycle events and keeps a [coredns-redis](https://github.com/mbartsch/coredns-redis) zone hash in sync. Each running container gets an A record `<container-name>.<zone>` pointing at its IP. Records are removed when containers stop or die.

## How it works

1. On startup, all running containers are registered and any stale records from a previous run are cleaned up.
2. A Docker event stream watches for `start`, `stop`, `die`, and `destroy` events and updates Redis in real time.
3. A periodic full re-sync runs every 5 minutes as a safety net.
4. Static records (SOA, NS, or any fixed entry) can be loaded from a YAML/JSON file and are re-applied on every sync and whenever the file changes.

## Configuration

All configuration is via environment variables.

| Variable              | Default                  | Description                                              |
|-----------------------|--------------------------|----------------------------------------------------------|
| `DOCKER_SOCKET`       | `/var/run/docker.sock`   | Path to the Docker Unix socket                           |
| `REDIS_ADDR`          | `127.0.0.1:6379`         | Redis/KeyDB address                                      |
| `REDIS_PASSWORD`      | _(empty)_                | Redis password (plain text)                              |
| `REDIS_PASSWORD_FILE` | _(empty)_                | Path to a file containing the Redis password             |
| `REDIS_PREFIX`        | `_dns:`                  | Key prefix used for the zone hash                        |
| `DNS_ZONE`            | `srv.bartsch.red.`       | Zone name (must end with `.`)                            |
| `HOSTNAME`            | system hostname          | Used to namespace the tracking set so multi-host setups don't conflict |
| `DNS_TTL`             | `30`                     | TTL in seconds for dynamically registered A records      |
| `DOCKER_NETWORK`      | _(empty)_                | If set, prefer the IP from this Docker network           |
| `STATIC_RECORDS_FILE` | _(empty)_                | Path to a YAML or JSON file of static DNS records        |

`REDIS_PASSWORD` and `REDIS_PASSWORD_FILE` are mutually exclusive — setting both is a fatal error.

## Static records file

When `STATIC_RECORDS_FILE` is set, dns-sync loads static records from a YAML or JSON file and writes them to Redis on startup, on every periodic re-sync, and whenever the file is modified (checked every 5 seconds). These records always overwrite whatever is currently in Redis, making the file the source of truth for static entries.

The file is a map of DNS name → [coredns-redis record object](https://github.com/sebageek/coredns-redis#record-format). Use `@` for the zone apex.

**`static-records.yaml`**
```yaml
"@":
  soa:
    ttl: 300
    mbox: "hostmaster.example.com."
    ns: "ns1.example.com."
    refresh: 300
    retry: 60
    expire: 3600
  ns:
    - host: "ns1.example.com."
      ttl: 300

ns1:
  a:
    - ttl: 300
      ip: "192.168.1.10"
```

JSON format is also accepted (auto-detected by file extension; unknown extensions try JSON then YAML).

If `STATIC_RECORDS_FILE` is not set, a minimal hardcoded SOA is written once at startup using `HSETNX` (will not overwrite an existing value).

## Building

```sh
docker build -f examples/Dockerfile -t dns-sync .
```

Or locally (requires Go 1.22+):

```sh
go build -o dns-sync .
```

## Running

See [`examples/docker-compose.yml`](examples/docker-compose.yml) for a full stack example including KeyDB and CoreDNS.

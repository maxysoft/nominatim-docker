# Nominatim with Varnish Cache

This Docker Compose configuration provides a production-ready setup for Nominatim with Varnish as a caching layer.

## Overview

The `docker-compose-varnish.yml` configuration includes:

- PostgreSQL with PostGIS (internal, not exposed)
- A one-shot Nominatim import container (full image; provisions and imports, then exits)
- Nominatim API server on the serve-only image (internal, not exposed; no import tooling, no admin credentials)
- An optional replication updater (full image; `--profile updates`)
- Varnish cache server (exposed on port 80)

## Key Features

### Security

- Only port 80 is exposed to the outside world (Varnish)
- PostgreSQL (5432) and Nominatim (8080) are not exposed, improving security
- Varnish filters requests to only allow GET and HEAD methods
- Server information headers are removed for security

### Caching Strategy

The Varnish configuration caches different Nominatim endpoints with appropriate TTLs for production use:

| Endpoint | Cache Duration | Rationale |
| ---------- | --------------- | ----------- |
| `/search` | 1 hour | Search results are relatively stable |
| `/reverse` | 6 hours | Reverse geocoding results are very stable |
| `/lookup` | 24 hours | OSM ID lookups rarely change |
| `/details` | 12 hours | Detail queries are stable |
| `/status` | 1 minute | Status should be relatively fresh |

### Performance Features

- Query string normalization (parameters are sorted for better cache hits)
- Cookie removal (Nominatim doesn't use cookies)
- Graceful degradation (serves stale content if backend is down)
- Cache headers are added to responses (X-Cache: HIT/MISS)
- 1GB memory allocation for Varnish cache

## Usage

### Starting the Stack

```bash
docker compose -f contrib/docker-compose-varnish.yml up
```

### Accessing the API

Once the import is complete, access the API through Varnish on port 80:

```bash
# Search query
curl "http://localhost/search.php?q=monaco"

# Reverse geocoding
curl "http://localhost/reverse.php?lat=43.7384&lon=7.4246"

# Check cache status
curl -I "http://localhost/search.php?q=monaco"
# Look for X-Cache: HIT or X-Cache: MISS header
```

### Accessing Varnish Stats

To view Varnish statistics:

```bash
docker exec nominatim-varnish varnishstat
```

### Purging the cache

Cached responses are not invalidated when replication updates the database. Ban them by hand; the
VCL sorts query parameters, so an exact URL has to be given in sorted form:

```bash
docker exec nominatim-varnish varnishadm "ban req.url ~ ^/search"
docker exec nominatim-varnish varnishadm "ban req.url == /search?limit=10&q=london"
docker exec nominatim-varnish varnishadm ban.list
curl -I "http://localhost/search?q=london&limit=10"  # expect X-Cache: MISS
```

## Customization

### Adjusting Cache TTLs

To modify cache durations, edit `contrib/varnish.vcl`:

```vcl
# Example: Change search cache from 1 hour to 30 minutes
if (bereq.url ~ "^/search" || bereq.url ~ "^/search\.php") {
    set beresp.ttl = 30m;  # Changed from 1h
    set beresp.http.Cache-Control = "public, max-age=1800";  # Changed from 3600
}
```

### Adjusting Varnish Memory

To change the amount of memory allocated to Varnish, edit `docker-compose-varnish.yml`:

```yaml
nominatim-varnish:
  environment:
    VARNISH_SIZE: 2G  # Increase from 1G to 2G
```

### Changing the Exposed Port

To expose Varnish on a different port, edit the ports section:

```yaml
nominatim-varnish:
  ports:
    - "8080:80"  # Expose on port 8080 instead of 80
```

## Production Considerations

1. **Cache Invalidation**: Cached responses live for their full TTL even after replication updates
   the database. Reduce the TTLs for frequently updated data, or ban stale entries (see
   [Purging the cache](#purging-the-cache)).
2. **Memory Sizing**: The default 1GB cache suits small to medium datasets. Size `VARNISH_SIZE` for
   your hit rate, checked with `varnishstat`.

## Troubleshooting

### Varnish not starting

A VCL that fails to compile leaves the container restarting, so `docker exec` cannot reach it.
Read the compile error from `docker logs nominatim-varnish`, or check the VCL in a one-off container:

```bash
docker compose -f contrib/docker-compose-varnish.yml run --rm --no-deps \
  --entrypoint varnishd nominatim-varnish -C -f /etc/varnish/default.vcl
```

### Low cache hit rate

- Check if query parameters are consistent
- Review Varnish logs: `docker logs nominatim-varnish`
- Verify requests are using GET method

### Backend connection issues

The Varnish 9 image ships neither curl nor wget. Check what Varnish sees, then the API itself:

```bash
docker exec nominatim-varnish varnishadm backend.list
docker compose -f contrib/docker-compose-varnish.yml exec nominatim nominatim-ctl healthcheck
```

## References

- [Nominatim API Documentation](https://nominatim.org/release-docs/latest/api/Overview/)
- [Varnish Cache Documentation](https://varnish-cache.org/docs/)
- [External PostGIS Setup](../docs/EXTERNAL-POSTGIS.md)

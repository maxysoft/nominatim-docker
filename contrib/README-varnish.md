# Nominatim with Varnish Cache

This Docker Compose configuration provides a production-ready setup for Nominatim with Varnish 8 as a caching layer.

## Overview

The `docker-compose-varnish.yml` configuration includes:

- PostgreSQL with PostGIS (internal, not exposed)
- Nominatim API server (internal, not exposed)
- Varnish 8 cache server (exposed on port 80)

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

### Monitoring Cache Performance

Check if requests are being cached:

```bash
# First request (cache MISS)
curl -I "http://localhost/search.php?q=monaco" | grep X-Cache
# Output: X-Cache: MISS

# Second request (cache HIT)
curl -I "http://localhost/search.php?q=monaco" | grep X-Cache
# Output: X-Cache: HIT
# Output: X-Cache-Hits: 1
```

### Accessing Varnish Stats

To view Varnish statistics:

```bash
docker exec nominatim-varnish varnishstat
```

## Configuration Files

- `docker-compose-varnish.yml` - Main Docker Compose configuration
- `varnish.vcl` - Varnish Cache Language configuration defining caching rules

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
varnish:
  environment:
    VARNISH_SIZE: 2G  # Increase from 1G to 2G
```

### Changing the Exposed Port

To expose Varnish on a different port, edit the ports section:

```yaml
varnish:
  ports:
    - "8080:80"  # Expose on port 8080 instead of 80
```

## Production Considerations

1. **Cache Invalidation**: The current setup doesn't automatically invalidate cache when database updates occur. For frequently updated databases, consider:
   - Reducing TTLs
   - Implementing a cache invalidation mechanism
   - Using replication webhooks to purge cache

2. **Memory Sizing**: The default 1GB cache is suitable for small to medium datasets. For larger deployments:
   - Increase `VARNISH_SIZE` based on available memory
   - Monitor cache hit rates with `varnishstat`
   - Aim for >80% cache hit rate

3. **Security**:
   - Change default passwords in the configuration
   - Consider adding rate limiting
   - Use HTTPS with a reverse proxy (nginx, traefik) in front of Varnish

4. **Monitoring**: Set up monitoring for:
   - Cache hit/miss ratios
   - Backend response times
   - Memory usage
   - Request rates

## Troubleshooting

### Varnish not starting

Check VCL syntax:

```bash
docker exec nominatim-varnish varnishd -C -f /etc/varnish/default.vcl
```

### Low cache hit rate

- Check if query parameters are consistent
- Review Varnish logs: `docker logs nominatim-varnish`
- Verify requests are using GET method

### Backend connection issues

Check Nominatim is accessible from Varnish:

```bash
docker exec nominatim-varnish wget -O- http://nominatim:8080/status.php
```

## References

- [Nominatim API Documentation](https://nominatim.org/release-docs/latest/api/Overview/)
- [Varnish Cache Documentation](https://varnish-cache.org/docs/)
- [External PostGIS Setup](../external-postgis.md)

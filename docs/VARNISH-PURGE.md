# Purging the Varnish cache (for the contrib/varnish.vcl configuration)

This document explains safe, correct and practical ways to purge/invalidate cached objects for the Varnish VCL you are using (the original file is here: https://github.com/maxysoft/nominatim-docker/blob/master/contrib/varnish.vcl).

It covers:
- Concepts: purge vs ban vs surrogate keys
- How to purge a single URL or patterns
- Docker-specific examples (docker exec / exposed management port)
- Verification steps and best practices

Keep in mind: the VCL canonicalizes query strings (so that cache keys match canonicalized URLs). Any purge you perform must target the same canonicalized form of the URL.

---

## Concepts: `purge`, `ban`, and surrogate keys

- `ban`: Varnish's built-in technique for marking cached objects invalid by a boolean expression (e.g. `ban req.url ~ "^/search"`). The object is not removed immediately from disk, but subsequent lookups skip banned objects; the backend object is re-fetched when needed.
- HTTP `PURGE`: not a built-in Varnish management command — many deployments implement an HTTP endpoint or VCL handler that triggers a backend `ban` (or uses a VMOD that exposes a purge function).
- Surrogate key / Surrogate-Control: tag objects with identifiers (e.g. `Surrogate-Key: search:xyz`) and ban by key. This is efficient when you want to invalidate a group of objects (e.g., all objects for a particular resource id).

Recommendation: Use `varnishadm ban` (or `varnishadm` called from a management script) for most invalidations, and adopt a Surrogate-Key tagging scheme to allow fine-grained invalidation by logical key.

---

## 1) Purge (invalidate) a single URL — using varnishadm (preferred)

1. Canonicalize the URL the same way VCL does. If you used std.querysort (or vmod-querystring) to sort query strings before hashing, you must pass the canonicalized form to the ban expression, or ban in a way that ignores parameter order.

2. Example: ban by URL prefix (all `/search` URLs)

- From a shell that has access to `varnishadm` inside the running varnish container:

  docker exec -it varnish varnishadm "ban req.url ~ ^/search"

- Or, using remote management (host machine) pointing to the management interface, and using the secret file if required:

  varnishadm -T 127.0.0.1:6082 -S /etc/varnish/secret "ban req.url ~ '^/search'"

Notes:
- Use single quotes around the expression if your shell would expand characters.
- `ban req.url ~ '^/search'` will match any URL whose path begins with `/search`. If your URLs include query strings and the VCL canonicalizes them, the ban regex should match the canonicalized form or match just the path portion: `ban req.url ~ '^/search(\?|$)'`.

3. Purge a specific, fully-canonicalized URL

- If your canonical VCL produces `"/search?q=a&limit=10"` (sorted), you can ban that exact URL:

  varnishadm -T 127.0.0.1:6082 -S /etc/varnish/secret "ban req.url == /search?q=a&limit=10"

- Or match exact URL with regex:

  varnishadm -T 127.0.0.1:6082 -S /etc/varnish/secret "ban req.url ~ '^/search\\?q=a&limit=10$'"

Caveat:
- `==` checks equality and is strict. Regex `~` is more flexible (remember to escape special chars).

---

## 2) Purge (invalidate) by pattern (ban expressions)

Ban expressions allow you to match by many fields:

- Path-based ban:

  varnishadm "ban req.url ~ '^/reverse'"

- Host + path ban:

  varnishadm "ban req.http.host == 'nominatim.example.org' && req.url ~ '^/search'"

- Ban using backend response header (surrogate key, see next section):

  varnishadm "ban obj.http.Surrogate-Key ~ 'place:12345'"

Useful commands:

- List active bans:

  varnishadm "ban.list"

- Remove all bans (dangerous) — there is no single "clear bans" command; you can script inspection and reloading of the VCL to reset state or restart Varnish depending on needs. Usually you do not clear bans; you let them expire / be replaced.

---



## 3) Docker examples

- Run `varnishadm` inside the container (easy & safe if you have access to the container):

  docker exec -it varnish varnishadm "ban req.url ~ '^/search'"

- Connect to management port from host (if exposed in compose): (example uses secret at `/etc/varnish/secret` inside container)

  varnishadm -T 127.0.0.1:6082 -S /path/on/host/secret "ban req.url ~ '^/search'"

- If `secret` file is inside the container only, you can copy it out first:

  docker cp varnish:/etc/varnish/secret ./varnish_secret
  varnishadm -T 127.0.0.1:6082 -S ./varnish_secret "ban req.url ~ '^/search'"

Important: do not expose the management port publicly.

---


## 4) Security considerations

- Never expose Varnish management port (6082) to the public internet.
- Require that HTTP purge endpoints be only accessible to trusted hosts or protected with authentication.
- When allowing `PURGE` via HTTP, implement rate limiting and logging to avoid abuse.
- Prefer using a small admin process (running on management network) that authenticates incoming requests and runs `varnishadm` to execute bans.

---

## 5) Troubleshooting & tips

- If banned objects still appear to be served:
  - Verify the ban expression matches the canonicalized cache key (if you canonicalize query strings, make sure the ban targets canonical form).
  - Use `ban.list` to confirm the ban was registered.
  - Check varnish logs for ban activity and for eviction/grace behavior.
- If `varnishadm` complains about connecting:
  - Ensure the management port is reachable and you are using the correct secret (`-S`) file.
- If you need immediate eviction rather than lazy ban:
  - Consider changing TTL/grace temporarily or purge by exact object reference; bans are the standard, safe mechanism for invalidation.
- Consider adding `Surrogate-Key` support because it scales well for logically related invalidation.

---

## Practical examples (copy/paste)

- Ban all search requests:

  docker exec -it varnish varnishadm "ban req.url ~ '^/search'"

- Ban an exact canonical URL:

  docker exec -it varnish varnishadm "ban req.url == /search?q=london&limit=10"

- Check ban list:

  docker exec -it varnish varnishadm "ban.list"

- Verify X-Cache header after ban:

  curl -I 'http://localhost/search?q=london'
  # expect first request after ban: X-Cache: MISS
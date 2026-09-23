vcl 4.1;
import std;

# Backend configuration - Nominatim service
backend default {
    .host = "nominatim";
    .port = "8080";
    .connect_timeout = 60s;
    .first_byte_timeout = 300s;
}

# Access control - only allow specific HTTP methods
sub vcl_recv {
    # Only allow GET and HEAD requests
    if (req.method != "GET" && req.method != "HEAD") {
        return (synth(405, "Method Not Allowed"));
    }

    # Remove any cookies from requests (Nominatim doesn't use them)
    unset req.http.Cookie;

    # Normalize query strings by sorting parameters
    set req.url = std.querysort(req.url);

    # Cache the API endpoints (their TTLs are set in vcl_backend_response) and
    # pass everything else. Each prefix also matches the legacy .php form.
    if (req.url ~ "^/(search|reverse|lookup|details|status)") {
        return (hash);
    }
    return (pass);
}

sub vcl_hash {
    # Nominatim localises names by Accept-Language, so it is part of the key,
    # normalised to lower case without whitespace. The built-in vcl_hash then
    # adds the URL and host.
    if (req.http.Accept-Language) {
        hash_data(std.tolower(regsuball(req.http.Accept-Language, "\s+", "")));
    }
}

sub vcl_backend_response {
    # Never cache a server error: one failed query would otherwise be served
    # for up to 24 hours after the backend recovers. A failed background
    # refresh keeps the stale object; otherwise a short hit-for-miss stops
    # concurrent requests from queueing behind the failing one.
    if (beresp.status >= 500) {
        if (bereq.is_bgfetch) {
            return (abandon);
        }
        set beresp.ttl = 10s;
        set beresp.uncacheable = true;
        return (deliver);
    }

    # Set cache TTL based on the request URL
    if (bereq.url ~ "^/search") {
        set beresp.ttl = 1h;
        set beresp.http.Cache-Control = "public, max-age=3600";
    }
    elsif (bereq.url ~ "^/reverse") {
        set beresp.ttl = 6h;
        set beresp.http.Cache-Control = "public, max-age=21600";
    }
    elsif (bereq.url ~ "^/lookup") {
        set beresp.ttl = 24h;
        set beresp.http.Cache-Control = "public, max-age=86400";
    }
    elsif (bereq.url ~ "^/details") {
        set beresp.ttl = 12h;
        set beresp.http.Cache-Control = "public, max-age=43200";
    }
    elsif (bereq.url ~ "^/status") {
        set beresp.ttl = 1m;
        set beresp.http.Cache-Control = "public, max-age=60";
    }
    else {
        # Don't cache by default
        set beresp.ttl = 0s;
        set beresp.http.Cache-Control = "no-cache, no-store, must-revalidate";
    }

    # Remove cookies from backend response (Nominatim doesn't use them)
    unset beresp.http.Set-Cookie;

    # Allow stale content to be served if backend is down
    set beresp.grace = 1h;

    return (deliver);
}

sub vcl_deliver {
    # Add header to indicate cache status
    if (obj.hits > 0) {
        set resp.http.X-Cache = "HIT";
        set resp.http.X-Cache-Hits = obj.hits;
    } else {
        set resp.http.X-Cache = "MISS";
    }

    # Remove backend server information for security
    unset resp.http.Server;
    unset resp.http.X-Powered-By;
    unset resp.http.Via;
}

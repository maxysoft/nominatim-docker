vcl 4.1;

# Backend configuration - Nominatim service
backend default {
    .host = "nominatim";
    .port = "8080";
    .connect_timeout = 60s;
    .first_byte_timeout = 300s;
    .between_bytes_timeout = 60s;
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

    # Define caching rules for different endpoints
    if (req.url ~ "^/search" || req.url ~ "^/search\.php") {
        # Cache search queries for 1 hour
        # Search results are relatively stable
        return (hash);
    }
    elsif (req.url ~ "^/reverse" || req.url ~ "^/reverse\.php") {
        # Cache reverse geocoding for 6 hours
        # Reverse geocoding results are very stable
        return (hash);
    }
    elsif (req.url ~ "^/lookup" || req.url ~ "^/lookup\.php") {
        # Cache lookup queries for 24 hours
        # OSM ID lookups are very stable
        return (hash);
    }
    elsif (req.url ~ "^/details" || req.url ~ "^/details\.php") {
        # Cache details queries for 12 hours
        return (hash);
    }
    elsif (req.url ~ "^/status" || req.url ~ "^/status\.php") {
        # Cache status for 1 minute only
        return (hash);
    }
    else {
        # Don't cache other requests
        return (pass);
    }
}

sub vcl_backend_response {
    # Set cache TTL based on the request URL
    if (bereq.url ~ "^/search" || bereq.url ~ "^/search\.php") {
        set beresp.ttl = 1h;
        set beresp.http.Cache-Control = "public, max-age=3600";
    }
    elsif (bereq.url ~ "^/reverse" || bereq.url ~ "^/reverse\.php") {
        set beresp.ttl = 6h;
        set beresp.http.Cache-Control = "public, max-age=21600";
    }
    elsif (bereq.url ~ "^/lookup" || bereq.url ~ "^/lookup\.php") {
        set beresp.ttl = 24h;
        set beresp.http.Cache-Control = "public, max-age=86400";
    }
    elsif (bereq.url ~ "^/details" || bereq.url ~ "^/details\.php") {
        set beresp.ttl = 12h;
        set beresp.http.Cache-Control = "public, max-age=43200";
    }
    elsif (bereq.url ~ "^/status" || bereq.url ~ "^/status\.php") {
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

    return (deliver);
}

# Handle errors gracefully
sub vcl_backend_error {
    # Serve stale content if available
    if (beresp.ttl + beresp.grace > 0s) {
        return (deliver);
    }
    
    return (deliver);
}

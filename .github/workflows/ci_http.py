"""HTTP helper for the CI assert-* scripts: standard library only, because the
runner's python3 has no third-party packages installed."""

import http.client
import json
import sys
import time
import urllib.error
import urllib.request


def get_json(url, attempts):
    """GET url and decode JSON, retrying refused connections and 502/503/504
    with exponential backoff (2 s doubling, capped at 120 s), as the API may
    still be importing. Exits non-zero on any other failure."""
    delay = 2
    for attempt in range(1, attempts + 1):
        try:
            with urllib.request.urlopen(url, timeout=60) as resp:
                return json.load(resp)
        except urllib.error.HTTPError as e:
            if e.code not in (502, 503, 504) or attempt == attempts:
                print(f'Endpoint returned status code {e.code}: {e.read()[:500]!r}')
                sys.exit(1)
        except (urllib.error.URLError, http.client.HTTPException, ConnectionError, TimeoutError) as e:
            if attempt == attempts:
                print(f'Endpoint unreachable after {attempts} attempts: {e}')
                sys.exit(1)
        time.sleep(delay)
        delay = min(delay * 2, 120)

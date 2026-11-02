"""Bounded, TLS-verified Proxifly downloader and untrusted-list parser."""
import ipaddress
import ssl
import sys
import urllib.request
from urllib.parse import urlsplit, unquote

SOURCE = 'https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/protocols/http/data.txt'
MAX_BYTES = 2 * 1024 * 1024
CACHE_SECONDS = 300


def parse_proxies(text):
    """Only literal, global unicast HTTP endpoints; no DNS rebinding or credentials."""
    candidates, seen = [], set()
    for line in text.splitlines():
        line = line.strip()
        if not line or any(ord(c) <= 32 or ord(c) == 127 for c in line):
            continue
        try:
            u = urlsplit(line)
            if (u.scheme != 'http' or u.username is not None or u.password is not None
                    or u.path not in ('', '/') or u.query or u.fragment):
                continue
            ip = ipaddress.ip_address(u.hostname or '')
            if (not ip.is_global or ip.is_multicast or ip.is_reserved
                    or ip.is_unspecified or ip.is_loopback or ip.is_link_local):
                continue
            if not u.port or not 1 <= u.port <= 65535:
                continue
            host = f'[{ip}]' if ip.version == 6 else str(ip)
            endpoint = f'http://{host}:{u.port}'
        except (ValueError, TypeError):
            continue
        if endpoint not in seen:
            seen.add(endpoint)
            candidates.append(endpoint)
    return candidates


class ProxyInputError(ValueError):
    pass


def parse_user_proxies(text):
    """Explicit user lists can include authenticated HTTP/HTTPS/SOCKS5 or local endpoints."""
    candidates, seen = [], set()
    for number, raw in enumerate(text.splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith('#'):
            continue
        try:
            u = urlsplit(line)
            if (u.scheme not in ('http', 'https', 'socks5') or not u.hostname
                    or not u.port or not 1 <= u.port <= 65535
                    or u.path not in ('', '/') or u.query or u.fragment
                    or any(ord(c) <= 32 or ord(c) == 127 for c in line)):
                raise ProxyInputError()
        except (ValueError, TypeError):
            raise ProxyInputError(f'Invalid proxy URL at line {number}; expected scheme://[user:password@]host:port') from None
        if any(c in unquote(u.username or '') + unquote(u.password or '') for c in ('\r', '\n', '\x00')):
            raise ProxyInputError(f'Invalid proxy credentials at line {number}')
        normalized = line.rstrip('/')
        if normalized not in seen:
            candidates.append(normalized)
            seen.add(normalized)
    if not candidates:
        raise ProxyInputError('Proxy input contains no candidates')
    return candidates


def download():
    # No environment proxy, no insecure TLS, no redirects to a changed source.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *args, **kwargs):
            return None
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({}), NoRedirect(),
        urllib.request.HTTPSHandler(context=ssl.create_default_context()))
    req = urllib.request.Request(SOURCE, headers={'User-Agent': 'proxy-browser-test/1.0'})
    with opener.open(req, timeout=8) as response:
        length = response.headers.get('Content-Length')
        if length and int(length) > MAX_BYTES:
            raise ValueError('Proxy list exceeds the 2 MiB limit')
        data = response.read(MAX_BYTES + 1)
    if len(data) > MAX_BYTES:
        raise ValueError('Proxy list exceeds the 2 MiB limit')
    data.decode('utf-8', errors='strict')
    return data


if __name__ == '__main__':
    # Parent enforces a wall-clock timeout and reaps this helper on every OS.
    try:
        sys.stdout.buffer.write(download())
    except Exception as exc:
        print(f'Proxy list download failed: {type(exc).__name__}', file=sys.stderr)
        sys.exit(1)

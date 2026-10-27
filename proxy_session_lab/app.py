#!/usr/bin/env python3
"""Finite synthetic browser sessions on Linux, macOS, and Windows."""
import argparse
import asyncio
from collections import Counter
from contextlib import suppress
import ipaddress
import json
import math
import os
from pathlib import Path
import random
import re
import socket
import sys
import time
from urllib.parse import urlsplit
import uuid

from .proxy_source import SOURCE, MAX_BYTES, CACHE_SECONDS, parse_proxies
from .telemetry import EventLog, Timings
from .runtime_support import (Resources, ResourceMonitor, ShutdownSignals,
                              cache_directory, configure_browser_path, descendants,
                              is_root, process_snapshot)

ROOT = Path(__file__).resolve().parent
HEADER = 'X-Synthetic-Test-Run'
MIB = 1024 ** 2


def safe_url(url):
    """Never print URL userinfo (including in downloaded endpoints)."""
    return re.sub(r'(https?://)[^/@\s]+@', r'\1***:***@', str(url))


def origin(url):
    u = urlsplit(url)
    return u.scheme.lower(), (u.hostname or '').lower(), u.port or (443 if u.scheme == 'https' else 80)


def valid_url(value):
    try:
        u = urlsplit(value)
        if (u.scheme not in ('http', 'https') or not u.hostname or not u.netloc
                or u.username is not None or u.password is not None
                or any(ord(c) <= 32 or ord(c) == 127 for c in value)
                or (u.port is not None and not 1 <= u.port <= 65535)):
            raise ValueError()
        u.hostname.encode('idna')
    except (ValueError, UnicodeError):
        raise argparse.ArgumentTypeError('A valid HTTP(S) URL without embedded credentials is required') from None
    return value


def positive(value):
    try:
        number = float(value)
        if not math.isfinite(number) or number <= 0:
            raise ValueError()
        return number
    except ValueError:
        raise argparse.ArgumentTypeError('A finite number greater than zero is required') from None


def nonnegative(value):
    if value == '0' or value == '0.0':
        return 0.0
    return positive(value)


def positive_int(value):
    try:
        number = int(value)
        if number <= 0:
            raise ValueError()
        return number
    except ValueError:
        raise argparse.ArgumentTypeError('An integer greater than zero is required') from None


def concurrency_arg(value):
    return 'auto' if value == 'auto' else positive_int(value)


def arguments(argv=None):
    p = argparse.ArgumentParser(description='Finite synthetic browser sessions with isolated contexts. Run as a non-root user.')
    p.add_argument('url', nargs='?', type=valid_url, help='Your own test target (HTTP/HTTPS)')
    p.add_argument('--sessions', type=positive_int, default=20, help='Total session attempt budget (default: 20)')
    p.add_argument('--concurrency', type=concurrency_arg, default='auto', help='Active session limit: number or auto (default: auto)')
    p.add_argument('--max-concurrency', type=positive_int, default=8, help='Maximum active contexts (default: 8)')
    p.add_argument('--navigation-rate', type=positive, default=1.0, help='Application-started main navigations per second (default: 1)')
    p.add_argument('--max-duration', type=positive, default=120.0, help='Total duration including preparation, in seconds (default: 120)')
    p.add_argument('--navigation-timeout', type=positive, default=30.0, help='Timeout per navigation, in seconds (default: 30)')
    p.add_argument('--settle-seconds', type=nonnegative, default=5.0, help='Wait after DOMContentLoaded, in seconds (default: 5)')
    p.add_argument('--headed', action='store_true', help='Show the Chromium window')
    p.add_argument('--probe-url', type=valid_url, help='Optional proxy diagnostics URL that you own')
    p.add_argument('--dry-run', action='store_true', help='Print configuration only; no network requests or browser')
    p.add_argument('--log-file', help='Append structured performance/events to a .jsonl file (default: no file)')
    a = p.parse_args(argv)
    if a.url is None:
        if not sys.stdin.isatty():
            p.error('A target URL is required in non-interactive mode')
        try:
            a.url = valid_url(input('Your target URL: ').strip())
        except (EOFError, argparse.ArgumentTypeError) as exc:
            p.error(str(exc))
    if a.concurrency != 'auto' and a.concurrency > a.max_concurrency:
        p.error('--concurrency must not exceed --max-concurrency')
    if a.log_file and Path(a.log_file).suffix != '.jsonl':
        p.error('--log-file must use the .jsonl extension')
    return a


class Controller:
    """Admission only: never kills active sessions to reduce resource pressure."""
    def __init__(self, args):
        self.auto = args.concurrency == 'auto'
        self.maximum = min(args.max_concurrency, args.sessions)
        self.limit = min(2 if self.auto else args.concurrency, self.maximum)
        self.high_cpu = 0
        self.last_change = time.monotonic()
        self.cooldown_until = 0.0
        self.resource_faults = 0

    def resource_error(self):
        self.resource_faults += 1
        self.cooldown_until = time.monotonic() + 10
        if self.auto:
            self.limit = max(1, self.limit // 2)
            self.last_change = time.monotonic()

    def update(self, r, active):
        old = self.limit
        self.high_cpu = self.high_cpu + 1 if r.cpu >= 85 else 0
        now = time.monotonic()
        if self.auto:
            if (self.high_cpu >= 3 or r.available < r.total * .25) and now - self.last_change >= 3:
                self.limit = max(1, self.limit - 1)
                self.cooldown_until = now + 5
                self.last_change = now
                self.high_cpu = 0
            elif (now - self.last_change >= 5 and now >= self.cooldown_until
                    and r.cpu < 75 and r.available > r.total * .25 + 256 * MIB
                    and active >= self.limit):
                self.limit = min(self.maximum, self.limit + 1)
                self.last_change = now
        return old != self.limit

    def admits(self, r):
        return (r.available >= r.total * .25 + 128 * MIB
                and self.high_cpu < 3 and time.monotonic() >= self.cooldown_until)


class NavigationGate:
    """No catch-up bursts. Probe and target page.goto share this rate limit."""
    def __init__(self, rate, stopping):
        self.interval = 1 / rate
        self.next_start = 0.0
        self.lock = asyncio.Lock()
        self.stopping = stopping
        self.starts = 0

    async def acquire(self):
        async with self.lock:
            delay = self.next_start - time.monotonic()
            if delay > 0:
                try:
                    await asyncio.wait_for(self.stopping.wait(), delay)
                except asyncio.TimeoutError:
                    pass
            if self.stopping.is_set():
                raise asyncio.CancelledError()
            self.next_start = time.monotonic() + self.interval
            self.starts += 1


def classify_error(exc):
    s = str(exc).upper()
    if isinstance(exc, MemoryError) or any(x in s for x in ('OUT_OF_MEMORY', 'ENOMEM', 'NOT ENOUGH MEMORY', 'TOO MANY OPEN FILES', 'RESOURCE_EXHAUSTED')):
        return 'resource_exhaustion'
    if 'ERR_NAME_NOT_RESOLVED' in s or 'NAME_RESOLUTION_FAILED' in s:
        return 'dns'
    if any(x in s for x in ('ERR_CERT_', 'ERR_SSL_', 'CERTIFICATE_VERIFY_FAILED')):
        return 'tls'
    if any(x in s for x in ('ERR_PROXY_', 'ERR_TUNNEL_CONNECTION_FAILED', 'PROXY AUTHENTICATION')):
        return 'proxy_connection'
    if isinstance(exc, TimeoutError) or type(exc).__name__ == 'TimeoutError' or 'ERR_TIMED_OUT' in s:
        return 'navigation_timeout'
    if any(x in s for x in ('CRASH', 'TARGET PAGE, CONTEXT OR BROWSER HAS BEEN CLOSED', 'BROWSER CLOSED', 'TARGET CLOSED')):
        return 'browser_context_crash'
    return 'network_navigation_error'


async def install_run_header(context, page, target, run_id):
    """CDP overrides only this request, not subsequent redirect hops.

    Unlike route.continue_(headers=...), CDP Fetch.continueRequest does not
    propagate overridden headers to a cross-origin redirect.
    """
    cdp = await context.new_cdp_session(page)
    tree = await cdp.send('Page.getFrameTree')
    main_frame = tree['frameTree']['frame']['id']
    pending = set()

    async def resume(event):
        request = event['request']
        headers = [{'name': k, 'value': str(v)} for k, v in request['headers'].items()
                   if k.lower() != HEADER.lower()]
        if event.get('frameId') == main_frame and origin(request['url']) == origin(target):
            headers.append({'name': HEADER, 'value': run_id})
        with suppress(Exception):
            await cdp.send('Fetch.continueRequest', {'requestId': event['requestId'], 'headers': headers})

    def paused(event):
        task = asyncio.create_task(resume(event))
        pending.add(task)
        task.add_done_callback(pending.discard)

    cdp.on('Fetch.requestPaused', paused)
    await cdp.send('Fetch.enable', {'patterns': [{'resourceType': 'Document', 'requestStage': 'Request'}]})
    return pending


async def load_candidates(cache_dir):
    cache = cache_dir / 'proxifly-http.txt'
    data = None
    if cache.is_file():
        stat = cache.stat()
        if 0 <= time.time() - stat.st_mtime < CACHE_SECONDS and stat.st_size <= MAX_BYTES:
            data = cache.read_bytes()
    source = 'cache' if data is not None else 'downloaded'
    if data is None:
        process = await asyncio.create_subprocess_exec(
            sys.executable, str(ROOT / 'proxy_source.py'),
            stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE)
        try:
            data, err = await asyncio.wait_for(process.communicate(), 13)
            if process.returncode:
                raise RuntimeError(err.decode(errors='replace').strip())
        finally:
            if process.returncode is None:
                with suppress(ProcessLookupError):
                    process.kill()
                await process.wait()
        if len(data) > MAX_BYTES:
            raise RuntimeError('Proxy list exceeds the size limit')
    candidates = parse_proxies(data.decode('utf-8', errors='strict'))
    if not candidates:
        raise RuntimeError('No valid global HTTP proxy candidates found')
    if source == 'downloaded':
        cache_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        tmp = cache.with_suffix(f'.{uuid.uuid4().hex}.tmp')
        try:
            with tmp.open('xb') as f:
                os.chmod(tmp, 0o600)
                f.write(data)
            tmp.replace(cache)
        finally:
            tmp.unlink(missing_ok=True)
    random.SystemRandom().shuffle(candidates)
    return candidates, source


async def preflight(url, log):
    host = urlsplit(url).hostname
    if host.lower().endswith(('.ts.net', '.localhost', '.local', '.test')) or host.lower() == 'localhost':
        log('WARNING: Local/private/Tailscale target; public internet proxies may not be able to reach it.')
        return
    try:
        addresses = {ipaddress.ip_address(host)}
    except ValueError:
        try:
            info = await asyncio.wait_for(asyncio.get_running_loop().getaddrinfo(host, None, type=socket.SOCK_STREAM), 2)
            addresses = {ipaddress.ip_address(row[4][0]) for row in info}
        except (TimeoutError, OSError):
            log('Preflight DNS check was inconclusive; proxy-side DNS may differ.')
            return
    if any(not ip.is_global for ip in addresses):
        log('WARNING: Target resolves to a local/private/CGNAT address; public proxies may not be able to reach it.')


async def diagnostic_ip(response, timeout):
    """Read only small, uncompressed JSON; compressed length is no decoded bound."""
    headers = response.headers
    length = headers.get('content-length', '')
    encoding = headers.get('content-encoding', '').strip().lower()
    if (encoding not in ('', 'identity') or not length.isdigit()
            or int(length) > 4096 or 'json' not in headers.get('content-type', '')):
        return None
    try:
        body = await asyncio.wait_for(response.body(), min(2, timeout))
        value = json.loads(body).get('ip') if len(body) <= 4096 else None
        return ipaddress.ip_address(value) if isinstance(value, str) else None
    except (ValueError, TypeError, AttributeError, asyncio.TimeoutError):
        return None


class Runner:
    def __init__(self, args, run_id=None, logger=None):
        self.args = args
        self.run_id = run_id or uuid.uuid4().hex
        self.log_sink = logger or print
        self.stopping = asyncio.Event()
        self.stop_reason = None
        self.counts = Counter()
        self.errors = Counter()
        self.network_errors = Counter()
        self.navigation_times = Timings()
        self.session_times = Timings()
        self.active = 0
        self.peak_active = 0
        self.reserved = 0
        self.controller = Controller(args)
        self.monitor = ResourceMonitor()
        self.resources = self.monitor.sample()
        self.gate = NavigationGate(args.navigation_rate, self.stopping)
        self.queue = asyncio.Queue(maxsize=self.controller.maximum)
        self.contexts = set()
        self.peak_browser_rss = 0
        self.peak_app_rss = self.resources.app_rss
        self.peak_helper_rss = 0
        self.peak_cpu = 0.0
        self.minimum_available = self.resources.available
        self.began = time.monotonic()
        self.events = EventLog(args.log_file, self.run_id)

    def log(self, message, event='message', **fields):
        self.log_sink(f'[{self.run_id}] {message}')
        self.events.emit(event, message=message, **fields)

    def stop(self, reason):
        if not self.stopping.is_set():
            self.stop_reason = reason
            self.log(f'STOP: {reason}')
            self.stopping.set()

    def sample_resources(self):
        self.resources = self.monitor.sample()
        self.peak_browser_rss = max(self.peak_browser_rss, self.resources.chromium_rss)
        self.peak_app_rss = max(self.peak_app_rss, self.resources.app_rss)
        self.peak_helper_rss = max(self.peak_helper_rss, self.resources.helper_rss)
        self.peak_cpu = max(self.peak_cpu, self.resources.cpu)
        self.minimum_available = min(self.minimum_available, self.resources.available)

    def performance(self):
        elapsed = max(time.monotonic() - self.began, .000001)
        resolved = self.counts['ok'] + self.counts['failed']
        return {
            'elapsed_s': elapsed,
            'success_percent': 100 * self.counts['ok'] / resolved if resolved else None,
            'started_per_s': self.counts['started'] / elapsed,
            'finished_per_s': self.counts['finished'] / elapsed,
            'queue_depth': self.queue.qsize(),
            'navigation': self.navigation_times.snapshot(),
            'session': self.session_times.snapshot(),
            'peak_cpu_percent': self.peak_cpu,
            'peak_app_rss_mib': self.peak_app_rss / MIB,
            'peak_chromium_rss_mib': self.peak_browser_rss / MIB,
            'peak_helper_rss_mib': self.peak_helper_rss / MIB,
            'min_available_ram_mib': self.minimum_available / MIB,
        }

    def status(self, final=False):
        r, c = self.resources, self.counts
        self.log(f'{"FINAL" if final else "STATUS"} planned={self.args.sessions} started={c["started"]} '
                 f'active={self.active} finished={c["finished"]} succeeded={c["ok"]} '
                 f'failed={c["failed"]} timeout={c["timeout"]} cancelled={c["cancelled"]} '
                 f'limit={self.controller.limit} navigations={self.gate.starts} '
                 f'RAM_available={r.available / MIB:.0f}MiB CPU={r.cpu:.0f}% '
                 f'RSS_app~{r.app_rss / MIB:.0f}MiB RSS_Chromium~{r.chromium_rss / MIB:.0f}MiB '
                 f'RSS_helpers~{r.helper_rss / MIB:.0f}MiB Chromium_processes={r.chromium_processes}',
                 event='status', final=final, counts=dict(c), active=self.active,
                 limit=self.controller.limit, navigations=self.gate.starts,
                 resources=vars(r))
        perf = self.performance()
        nav = perf['navigation']
        median = f'{nav["median_s"]:.2f}' if nav['median_s'] is not None else '-'
        p95 = f'{nav["p95_s"]:.2f}' if nav['p95_s'] is not None else '-'
        success = f'{perf["success_percent"]:.1f}%' if perf['success_percent'] is not None else '-'
        self.log(f'PERF elapsed_s={perf["elapsed_s"]:.1f} success={success} '
                 f'finished_per_s={perf["finished_per_s"]:.2f} queue={perf["queue_depth"]} '
                 f'nav_median_s={median} nav_p95_s={p95} nav_window={nav["window_count"]} '
                 f'CPU_peak={self.peak_cpu:.0f}% RSS_app_peak~{self.peak_app_rss / MIB:.0f}MiB '
                 f'RSS_Chromium_peak~{self.peak_browser_rss / MIB:.0f}MiB',
                 event='performance', final=final, **perf)

    async def watch(self):
        next_report = time.monotonic()
        while not self.stopping.is_set():
            self.sample_resources()
            if self.controller.update(self.resources, self.active):
                self.log(f'Auto admission limit -> {self.controller.limit} (CPU/RAM/observed active sessions)')
            if time.monotonic() >= next_report:
                self.status()
                next_report = time.monotonic() + 5
            await asyncio.sleep(1)

    async def navigate(self, page, url, role='target'):
        await self.gate.acquire()
        start = time.monotonic()
        response = None
        try:
            response = await page.goto(url, wait_until='domcontentloaded', timeout=self.args.navigation_timeout * 1000)
            return response, time.monotonic() - start
        finally:
            duration = time.monotonic() - start
            self.navigation_times.observe(duration)
            self.events.emit('navigation', role=role, duration_s=duration,
                             http_status=response.status if response else None)

    async def close_session(self, context, header_tasks):
        if context is not None:
            try:
                await asyncio.wait_for(context.close(), 3)
            except Exception:
                self.stop('Context cleanup failed; closing Chromium')
            finally:
                self.contexts.discard(context)
        pending = tuple(header_tasks)
        for task in pending:
            task.cancel()
        if pending:
            await asyncio.gather(*pending, return_exceptions=True)

    async def session(self, browser, number, proxy):
        context = None
        header_tasks = set()
        started = time.monotonic()
        nav_seconds = None
        status = None
        outcome = 'cancelled'
        phase = 'context_setup'
        network_error = None
        self.counts['started'] += 1
        self.active += 1
        self.peak_active = max(self.peak_active, self.active)
        try:
            # Chromium otherwise bypasses the configured proxy for loopback URLs.
            context = await browser.new_context(
                proxy={'server': proxy, 'bypass': '<-loopback>'}, ignore_https_errors=False)
            self.contexts.add(context)
            page = await context.new_page()
            header_tasks = await install_run_header(context, page, self.args.url, self.run_id)

            def response_received(response):
                nonlocal status
                req = response.request
                if req.is_navigation_request() and req.frame == page.main_frame:
                    status = response.status
                    if status in (429, 503):
                        self.stop(f'Main response HTTP {status}; stopping without rotating to another proxy')
            page.on('response', response_received)

            if self.args.probe_url:
                phase = 'probe'
                probe, _ = await self.navigate(page, self.args.probe_url, role='probe')
                if probe is None or probe.status >= 400:
                    outcome = 'probe_http_error'
                    self.counts['failed'] += 1
                    self.errors[outcome] += 1
                    return
                # Bounded JSON is optional. A configured proxy alone proves no egress IP.
                ip = await diagnostic_ip(probe, self.args.navigation_timeout)
                if ip is not None:
                    self.log(f'session={number} diagnostic_reported_egress_ip={ip}')
                else:
                    self.log(f'session={number} probe response received; egress IP not verified')
                # Reset probe-origin storage before the measured target navigation.
                await context.clear_cookies()
                probe_cdp = await context.new_cdp_session(page)
                probe_origin = urlsplit(self.args.probe_url)
                await probe_cdp.send('Storage.clearDataForOrigin', {
                    'origin': f'{probe_origin.scheme}://{probe_origin.netloc}', 'storageTypes': 'all'})
                await probe_cdp.detach()
                status = None
            phase = 'target_navigation'
            response, nav_seconds = await self.navigate(page, self.args.url)
            status = response.status if response else status
            if response is None or status >= 400:
                outcome = 'target_http_error' if response else 'no_main_response'
                self.counts['failed'] += 1
                self.errors[outcome] += 1
            else:
                phase = 'settle'
                await asyncio.sleep(self.args.settle_seconds)
                outcome = 'navigation_succeeded'
                self.counts['ok'] += 1
        except asyncio.CancelledError:
            self.counts['cancelled'] += 1
            if status in (429, 503):
                outcome = 'target_http_error'
                self.counts['failed'] += 1
                self.counts['cancelled'] -= 1
                self.errors[outcome] += 1
            raise
        except Exception as exc:
            outcome = classify_error(exc)
            self.errors[outcome] += 1
            self.counts['failed'] += 1
            if outcome == 'navigation_timeout':
                self.counts['timeout'] += 1
            if outcome == 'resource_exhaustion':
                self.controller.resource_error()
            if outcome == 'browser_context_crash' and not browser.is_connected():
                self.stop('Chromium connection closed')
            # Error text can contain credentials, HTML, or huge call logs. Emit only
            # the structured category and Chromium network error code, if any.
            match = re.search(r'net::(ERR_[A-Z0-9_]+)', str(exc))
            if match:
                network_error = match[1]
                self.network_errors[network_error] += 1
                outcome += f'({match[1]})'
        finally:
            # Keep close and bookkeeping alive even if another cancellation
            # arrives after the session has already entered its finally block.
            cleanup = asyncio.create_task(self.close_session(context, header_tasks))
            cancelled_during_cleanup = False
            try:
                while not cleanup.done():
                    try:
                        await asyncio.shield(cleanup)
                    except asyncio.CancelledError:
                        cancelled_during_cleanup = True
                cleanup.result()
            finally:
                self.active -= 1
                self.counts['finished'] += 1
                duration = time.monotonic() - started
                self.session_times.observe(duration)
                nav_text = f'{nav_seconds:.2f}' if nav_seconds is not None else '-'
                self.log(f'session={number} proxy={safe_url(proxy)} HTTP={status or "-"} '
                         f'result={outcome} phase={phase} navigation_s={nav_text} '
                         f'session_s={duration:.2f} active={self.active}',
                         event='session', session_number=number, proxy=safe_url(proxy),
                         http_status=status, result=outcome, phase=phase,
                         network_error=network_error, navigation_s=nav_seconds,
                         duration_s=duration, active=self.active)
            if cancelled_during_cleanup:
                raise asyncio.CancelledError()

    async def worker(self, browser):
        while True:
            number, proxy = await self.queue.get()
            try:
                if self.stopping.is_set():
                    return
                await self.session(browser, number, proxy)
            finally:
                self.reserved -= 1
                self.queue.task_done()

    async def schedule(self, browser, candidates):
        # Queue holds at most max-concurrency. No per-session task explosion.
        budget = min(self.args.sessions, len(candidates))
        if budget < self.args.sessions:
            self.log(f'Only {budget} distinct proxies available; candidates will not be reused')
        workers = [asyncio.create_task(self.worker(browser)) for _ in range(self.controller.maximum)]
        watch = asyncio.create_task(self.watch())
        index = 0
        try:
            while not self.stopping.is_set():
                # A failed worker must stop the scheduler, not silently exhaust
                # the pool while admission waits forever.
                for task in [*workers, watch]:
                    if task.done() and not task.cancelled() and task.exception():
                        raise task.exception()
                if index >= budget and self.reserved == 0:
                    break
                if (index < budget and self.reserved < self.controller.limit
                        and self.controller.admits(self.resources)):
                    self.reserved += 1
                    self.queue.put_nowait((index + 1, candidates[index]))
                    index += 1
                else:
                    await asyncio.sleep(.05)
        finally:
            for task in workers + [watch]:
                task.cancel()
            await asyncio.gather(*workers, watch, return_exceptions=True)
            while not self.queue.empty():
                self.queue.get_nowait()
                self.queue.task_done()
                self.reserved -= 1

    async def execute(self, candidates=None, precheck=True):
        """candidates injection is an internal seam for loopback-only tests."""
        configure_browser_path()
        from playwright.async_api import async_playwright
        self.log(f'target={safe_url(self.args.url)} synthetic_sessions={self.args.sessions} '
                 f'initial_concurrency={self.controller.limit} rate={self.args.navigation_rate}/s',
                 event='run_started', target=safe_url(self.args.url), config=vars(self.args))
        self.log('Summed RSS includes shared pages; it is not exact physical RAM consumption.')
        browser = None
        try:
            if precheck:
                await preflight(self.args.url, self.log)
            if candidates is None:
                candidates, source = await load_candidates(cache_directory())
                self.log(f'Proxifly {source}: {len(candidates)} distinct unverified candidates')
            async with async_playwright() as playwright:
                try:
                    browser = await playwright.chromium.launch(
                        headless=not self.args.headed, chromium_sandbox=True,
                        handle_sigint=False, handle_sigterm=False, handle_sighup=False,
                        timeout=min(30, self.args.max_duration) * 1000)
                    await self.schedule(browser, candidates)
                finally:
                    self.sample_resources()
                    if browser:
                        await asyncio.wait_for(browser.close(), 5)
        finally:
            self.sample_resources()

    def summary(self):
        self.status(final=True)
        self.log(f'reason={self.stop_reason or "session budget/proxy candidates exhausted"} '
                 f'elapsed_s={time.monotonic() - self.began:.2f} peak_active={self.peak_active} '
                 f'peak_Chromium_RSS~{self.peak_browser_rss / MIB:.0f}MiB '
                 f'min_RAM_available={self.minimum_available / MIB:.0f}MiB errors={dict(self.errors)} '
                 f'network_errors={dict(self.network_errors)}',
                 event='summary', reason=self.stop_reason or 'session budget/proxy candidates exhausted',
                 counts=dict(self.counts), active=self.active, peak_active=self.peak_active,
                 errors=dict(self.errors), network_errors=dict(self.network_errors),
                 performance=self.performance())
        self.log('Navigation success does not prove a counter increment or a real person/device visit. '
                 'Without real-target measurements, start with 2 active synthetic sessions.')


async def run(args):
    runner = Runner(args)
    try:
        with ShutdownSignals(asyncio.get_running_loop(), runner.stop):
            return await run_until_stopped(runner)
    finally:
        runner.events.close()


async def run_until_stopped(runner):
    args = runner.args
    execution = asyncio.create_task(runner.execute())
    stop_wait = asyncio.create_task(runner.stopping.wait())
    try:
        done, _ = await asyncio.wait((execution, stop_wait), timeout=args.max_duration,
                                     return_when=asyncio.FIRST_COMPLETED)
        if not done:
            runner.stop('max-duration')
        if execution not in done:
            execution.cancel()
        result = (await asyncio.gather(execution, return_exceptions=True))[0]
        if isinstance(result, BaseException) and not isinstance(result, asyncio.CancelledError):
            runner.stop(f'Setup/runtime error: {type(result).__name__}')
            runner.log('Check the Chromium installation, system libraries, sandbox, and Proxifly connectivity.')
            return 2
        if runner.stop_reason in ('SIGINT', 'SIGBREAK', 'SIGTERM', 'SIGHUP'):
            return 130 if runner.stop_reason in ('SIGINT', 'SIGBREAK') else 143
        if runner.stop_reason and runner.stop_reason != 'max-duration':
            return 2
        return 1 if runner.counts['failed'] else 0
    finally:
        stop_wait.cancel()
        await asyncio.gather(stop_wait, return_exceptions=True)
        runner.summary()


def main():
    args = arguments()
    if args.dry_run:
        config = vars(args).copy()
        config['run_id'] = uuid.uuid4().hex
        config['proxy_source'] = SOURCE
        config['initial_concurrency'] = min(2 if args.concurrency == 'auto' else args.concurrency,
                                            args.max_concurrency, args.sessions)
        config['ram_reserve_percent'] = 25
        config['network_requests'] = 0
        print(json.dumps(config, ensure_ascii=False, indent=2))
        return 0
    if is_root():
        print('Do not run this application as root; use a regular user account.', file=sys.stderr)
        return 2
    try:
        return asyncio.run(run(args))
    except OSError as exc:
        print(f'File/runtime I/O error: {exc}', file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())

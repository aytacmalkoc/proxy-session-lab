"""Offline proof: local HTTP fixtures/proxies + real sandboxed Chromium.

No public proxy or third-party test target is contacted. The fixture proxy only
serves synthetic.test / third.test and refuses every other destination.
"""
import asyncio
from contextlib import suppress
import io
import json
import os
from pathlib import Path
import signal
import ssl
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from proxy_session_lab import app
from proxy_session_lab.proxy_source import parse_proxies
from proxy_session_lab.telemetry import EventLog, Timings


def options(*items):
    return app.arguments(['http://synthetic.test/test', '--sessions', '4',
                          '--concurrency', '2', '--max-concurrency', '2',
                          '--navigation-rate', '4', '--settle-seconds', '.4',
                          '--max-duration', '15', *items])


class LocalProxy:
    def __init__(self, behavior='normal'):
        self.behavior = behavior
        self.records = []
        self.handlers = set()
        self.server = None
        self.connections = 0
        self.tunnel_port = None

    async def start(self):
        self.server = await asyncio.start_server(self.handle, '127.0.0.1', 0)
        self.endpoint = f'http://127.0.0.1:{self.server.sockets[0].getsockname()[1]}'
        return self

    async def close(self):
        self.server.close()
        await self.server.wait_closed()
        for task in list(self.handlers):
            task.cancel()
        await asyncio.gather(*self.handlers, return_exceptions=True)

    async def handle(self, reader, writer):
        task = asyncio.current_task()
        self.handlers.add(task)
        self.connections += 1
        try:
            raw = await asyncio.wait_for(reader.readuntil(b'\r\n\r\n'), 5)
            lines = raw.decode('latin1').split('\r\n')
            method, url, _ = lines[0].split(' ', 2)
            headers = dict(line.split(': ', 1) for line in lines[1:] if ': ' in line)
            headers = {k.lower(): v for k, v in headers.items()}
            from urllib.parse import urlsplit
            if method == 'CONNECT' and url == 'synthetic.test:443' and self.tunnel_port:
                self.records.append((time.monotonic(), url, headers))
                upstream_reader, upstream_writer = await asyncio.open_connection('127.0.0.1', self.tunnel_port)
                writer.write(b'HTTP/1.1 200 Connection established\r\n\r\n')
                await writer.drain()
                async def copy(source, destination):
                    try:
                        while block := await source.read(65536):
                            destination.write(block)
                            await destination.drain()
                    except ConnectionError:
                        pass
                    finally:
                        destination.close()
                try:
                    await asyncio.gather(copy(reader, upstream_writer), copy(upstream_reader, writer))
                finally:
                    upstream_writer.close()
                return
            u = urlsplit(url)
            if u.hostname not in ('synthetic.test', 'third.test'):
                writer.write(b'HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n')
                await writer.drain()
                return
            self.records.append((time.monotonic(), url, headers))
            if self.behavior == 'hang':
                await reader.read()
                return
            status = int(self.behavior) if self.behavior in ('429', '503', '500') else 200
            extra = ''
            if self.behavior == 'redirect' and u.hostname == 'synthetic.test':
                status = 302
                extra = 'Location: http://third.test/landing\r\n'
            if u.path == '/probe':
                body = b'{"ip":"203.0.113.7"}'
                content_type = 'application/json'
            else:
                content_type = 'text/html'
                body = b'''<html><script>
                  const previous = localStorage.getItem('seen');
                  const cookie = document.cookie;
                  localStorage.setItem('seen', 'yes'); document.cookie='seen=yes';
                  fetch('/evidence?storage=' + previous + '&cookie=' + encodeURIComponent(cookie));
                  </script><img src="http://third.test/pixel"></html>'''
                if u.path.startswith('/evidence') or u.path == '/pixel':
                    body = b'ok'
            reply = (f'HTTP/1.1 {status} Fixture\r\nContent-Type: {content_type}\r\n'
                     f'Content-Length: {len(body)}\r\nConnection: close\r\n{extra}\r\n').encode()
            writer.write(reply + body)
            await writer.drain()
        except (asyncio.IncompleteReadError, ConnectionError, TimeoutError):
            pass
        finally:
            writer.close()
            with suppress(Exception):
                await writer.wait_closed()
            self.handlers.discard(task)


def surviving_browser_pids(runner):
    snap = app.process_snapshot()
    return [pid for pid, birth in runner.monitor.browser_identities
            if pid in snap and snap[pid][1] == birth]


class UnitTests(unittest.TestCase):
    def test_untrusted_proxy_parser(self):
        good = 'http://8.8.8.8:8080'
        entries = [good, good + '/', 'http://127.0.0.1:80', 'http://10.0.0.2:9',
                   'http://100.64.0.1:80', 'http://169.254.1.2:80', 'http://[::1]:80',
                   'http://224.0.0.1:80', 'http://0.0.0.0:80', 'http://localhost:80',
                   'https://8.8.8.8:80', 'socks5://8.8.8.8:80', 'http://8.8.8.8:65536',
                   'http://user:secret@8.8.8.8:80', 'http://8.8.8.8:80/payload',
                   'http://8.8.8.8:80?x=1', '$(touch /tmp/bad)', 'http://8.8.8.8:0',
                   'http://[::ffff:127.0.0.1]:80', 'http://8.8.8.8:80\tbad']
        self.assertEqual(parse_proxies('\n'.join(entries)), [good])

    def test_arguments(self):
        with patch('sys.stdin', io.StringIO('')), suppress_stderr():
            with self.assertRaises(SystemExit):
                app.arguments([])
        for args in [('--navigation-rate', 'nan'), ('--sessions', '0'),
                     ('--concurrency', '9'), ('--max-duration', 'inf')]:
            with suppress_stderr(), self.assertRaises(SystemExit):
                options(*args)
        self.assertEqual(app.valid_url('https://example.test/'), 'https://example.test/')

    def test_auto_controller_resource_evidence(self):
        c = app.Controller(options('--concurrency', 'auto', '--max-concurrency', '8', '--sessions', '20'))
        r = app.Resources(total=32_000 * app.MIB, available=24_000 * app.MIB, cpu=10)
        self.assertEqual(c.limit, 2)
        c.last_change -= 6
        c.update(r, 2)
        self.assertEqual(c.limit, 3)
        r.cpu = 95
        c.last_change -= 6
        for _ in range(3):
            c.update(r, 3)
        self.assertEqual(c.limit, 2)
        r.available = 7_000 * app.MIB
        self.assertFalse(c.admits(r))
        c.resource_error()
        self.assertEqual(c.limit, 1)
        self.assertEqual(app.classify_error(Exception('net::ERR_PROXY_CONNECTION_FAILED')), 'proxy_connection')
        self.assertEqual(app.classify_error(TimeoutError()), 'navigation_timeout')
        self.assertEqual(c.resource_faults, 1)

    def test_dry_run_cannot_start_network_or_browser(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'dry-run.jsonl'
            with patch.object(sys, 'argv', ['app.py', 'https://synthetic.test/test', '--dry-run', '--log-file', str(path)]), \
                 patch('socket.socket', side_effect=AssertionError('Network attempted')), \
                 patch.object(app, 'run', side_effect=AssertionError('Browser attempted')), \
                 patch('sys.stdout', io.StringIO()) as output:
                self.assertEqual(app.main(), 0)
            self.assertFalse(path.exists())
        self.assertEqual(json.loads(output.getvalue())['network_requests'], 0)

    def test_bounded_timings_keep_lifetime_mean_and_recent_percentiles(self):
        timings = Timings(capacity=3)
        for value in (1, 2, 3, 4, 5):
            timings.observe(value)
        self.assertEqual(timings.snapshot(), {
            'count': 5, 'window_count': 3, 'mean_s': 3.0, 'min_s': 1,
            'max_s': 5, 'median_s': 4, 'p95_s': 5})
        self.assertIsNone(Timings().snapshot()['p95_s'])

    def test_jsonl_appends_runs_with_utc_timestamps(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'events.jsonl'
            for run_id in ('first', 'second'):
                log = EventLog(path, run_id)
                log.emit('fixture', value=3)
                log.close()
            records = [json.loads(line) for line in path.read_text().splitlines()]
            self.assertEqual([r['run_id'] for r in records], ['first', 'second'])
            self.assertTrue(all(r['timestamp'].endswith('+00:00') for r in records))
            self.assertTrue(all(r['elapsed_s'] >= 0 for r in records))
            if os.name != 'nt':
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)


class suppress_stderr:
    def __enter__(self):
        self.patch = patch('sys.stderr', io.StringIO())
        return self.patch.__enter__()
    def __exit__(self, *args):
        return self.patch.__exit__(*args)


class CleanupRegressionTests(unittest.IsolatedAsyncioTestCase):
    async def test_cancellation_during_close_preserves_cleanup_and_counts(self):
        for cancel_in_body in (False, True):
            with self.subTest(cancel_in_body=cancel_in_body):
                closing, release, opened = asyncio.Event(), asyncio.Event(), asyncio.Event()
                class Page:
                    def on(self, *args): pass
                    async def goto(self, *args, **kwargs):
                        opened.set()
                        return SimpleNamespace(status=200)
                class Context:
                    closed = False
                    async def new_page(self): return Page()
                    async def close(self):
                        closing.set()
                        await release.wait()
                        self.closed = True
                context = Context()
                class Browser:
                    async def new_context(self, **kwargs): return context
                async def header(*args): return set()
                runner = app.Runner(options('--sessions', '1', '--settle-seconds',
                                            '60' if cancel_in_body else '0'), logger=lambda _: None)
                with patch.object(app, 'install_run_header', header):
                    task = asyncio.create_task(runner.session(Browser(), 1, 'http://fixture.invalid:8000'))
                    if cancel_in_body:
                        await asyncio.wait_for(opened.wait(), 1)
                        task.cancel()
                    await asyncio.wait_for(closing.wait(), 1)
                    task.cancel()
                    await asyncio.sleep(0)
                    # Repeated cancellation cannot abandon the in-flight close.
                    task.cancel()
                    release.set()
                    await asyncio.wait_for(asyncio.gather(task, return_exceptions=True), 1)
                self.assertTrue(context.closed)
                self.assertEqual(runner.active, 0)
                self.assertEqual(len(runner.contexts), 0)
                self.assertEqual(runner.counts['started'], 1)
                self.assertEqual(runner.counts['finished'], 1)
                self.assertEqual(runner.counts['cancelled'], int(cancel_in_body))
                self.assertEqual(runner.counts['ok'], int(not cancel_in_body))


class BrowserTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.proxies = []

    async def asyncTearDown(self):
        for proxy in self.proxies:
            await proxy.close()

    async def proxy(self, behavior='normal'):
        p = await LocalProxy(behavior).start()
        self.proxies.append(p)
        return p

    async def execute(self, args, proxies):
        from playwright.async_api import BrowserType
        logs = []
        runner = app.Runner(args, logger=logs.append)
        original_launch = BrowserType.launch
        launches = []
        async def counted_launch(browser_type, *args, **kwargs):
            launches.append(kwargs)
            return await original_launch(browser_type, *args, **kwargs)
        with patch.object(BrowserType, 'launch', counted_launch):
            await asyncio.wait_for(runner.execute([p.endpoint for p in proxies], precheck=False), 15)
        self.assertEqual(len(launches), 1)
        self.assertTrue(launches[0]['chromium_sandbox'])
        self.assertEqual(runner.active, 0)
        self.assertEqual(len(runner.contexts), 0)
        self.assertEqual(surviving_browser_pids(runner), [])
        return runner, logs

    async def test_isolation_proxy_assignment_header_rate_and_cleanup(self):
        proxies = [await self.proxy() for _ in range(4)]
        runner, logs = await self.execute(options(), proxies)
        self.assertEqual(runner.counts['started'], 4)
        self.assertEqual(runner.counts['ok'], 4)
        self.assertLessEqual(runner.peak_active, 2)
        starts = []
        for p in proxies:
            navigation = [r for r in p.records if r[1] == 'http://synthetic.test/test']
            self.assertEqual(len(navigation), 1, p.records)
            starts.append(navigation[0][0])
            self.assertEqual(navigation[0][2][app.HEADER.lower()], runner.run_id)
            evidence = [r for r in p.records if '/evidence?' in r[1]]
            self.assertTrue(evidence)
            self.assertTrue(all('storage=null&cookie=' in r[1] for r in evidence), evidence)
            self.assertTrue(all(app.HEADER.lower() not in r[2] for r in p.records if r not in navigation))
        starts.sort()
        self.assertTrue(all(b - a >= .22 for a, b in zip(starts, starts[1:])), starts)
        print(f'LOCAL_METRICS peak_active={runner.peak_active} browser_RSS_peak_MiB={runner.peak_browser_rss/app.MIB:.1f} '
              f'available_min_MiB={runner.minimum_available/app.MIB:.1f} navigation_starts={starts}')

    async def test_cross_origin_redirect_header_does_not_leak(self):
        p = await self.proxy('redirect')
        runner, _ = await self.execute(options('--sessions', '1'), [p])
        self.assertEqual(runner.counts['ok'], 1)
        landing = [r for r in p.records if r[1] == 'http://third.test/landing']
        self.assertTrue(landing, p.records)
        self.assertNotIn(app.HEADER.lower(), landing[0][2])

    async def test_target_429_and_503_stop_without_proxy_rotation(self):
        for code in ('429', '503'):
            p = await self.proxy(code)
            other = await self.proxy()
            runner, _ = await self.execute(options('--concurrency', '1'), [p, other])
            self.assertEqual(runner.counts['started'], 1)
            self.assertIn(code, runner.stop_reason)
            self.assertEqual(other.connections, 0)
            self.assertEqual(runner.counts['failed'], 1)

    async def test_failure_and_timeout_consume_budget_without_reuse(self):
        hang = await self.proxy('hang')
        good = await self.proxy()
        runner, _ = await self.execute(options('--sessions', '2', '--concurrency', '1',
                                              '--navigation-timeout', '.5'), [hang, good])
        self.assertEqual(runner.counts['started'], 2)
        self.assertEqual(runner.counts['timeout'], 1)
        self.assertEqual(runner.counts['ok'], 1)
        navs = [r for r in hang.records if r[1] == 'http://synthetic.test/test']
        self.assertEqual(len(navs), 1)
        # A refused local endpoint: one failed session, then one independent candidate.
        bad = await self.proxy()
        await bad.close()
        runner = app.Runner(options('--sessions', '2', '--concurrency', '1'), logger=lambda _: None)
        await asyncio.wait_for(runner.execute([bad.endpoint, good.endpoint], precheck=False), 15)
        self.assertEqual(runner.errors['proxy_connection'], 1)
        self.assertEqual(runner.counts['started'], 2)
        self.assertEqual(surviving_browser_pids(runner), [])

    async def test_probe_and_target_share_navigation_rate(self):
        p = await self.proxy()
        runner, logs = await self.execute(options('--sessions', '1', '--probe-url', 'http://synthetic.test/probe'), [p])
        starts = [r[0] for r in p.records if r[1] in ('http://synthetic.test/probe', 'http://synthetic.test/test')]
        self.assertEqual(len(starts), 2)
        self.assertGreaterEqual(starts[1] - starts[0], .22)
        self.assertTrue(any('203.0.113.7' in line for line in logs))

    async def test_structured_log_contains_real_navigation_and_performance(self):
        proxy = await self.proxy()
        bad = await self.proxy()
        await bad.close()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'run.jsonl'
            args = options('--sessions', '2', '--log-file', str(path))
            runner = app.Runner(args, logger=lambda _: None)
            try:
                await runner.execute([proxy.endpoint, bad.endpoint], precheck=False)
                runner.summary()
            finally:
                runner.events.close()
            records = [json.loads(line) for line in path.read_text().splitlines()]
            summary = next(r for r in records if r['event'] == 'summary')
            self.assertEqual(summary['active'], 0)
            self.assertEqual(summary['counts']['ok'], 1)
            self.assertEqual(summary['counts']['failed'], 1)
            self.assertEqual(summary['performance']['navigation']['count'], 2)
            self.assertEqual(summary['performance']['success_percent'], 50.0)
            self.assertEqual(summary['performance']['session']['count'], 2)
            self.assertTrue(summary['network_errors'])
            self.assertTrue(all(r['run_id'] == runner.run_id for r in records))
            self.assertEqual(len([r for r in records if r['event'] == 'navigation']), 2)
            self.assertEqual(len([r for r in records if r['event'] == 'session']), 2)
            self.assertEqual(surviving_browser_pids(runner), [])

    async def test_localhost_does_not_silently_bypass_proxy(self):
        p = await self.proxy()
        # Fixture refuses this address. If Chromium bypassed the proxy, there
        # would be no recorded connection and potentially a direct local hit.
        runner, _ = await self.execute(options('--sessions', '1', '--navigation-timeout', '2'), [p])
        self.assertEqual(runner.counts['ok'], 1)
        args = options('--sessions', '1', '--navigation-timeout', '2')
        direct = await self.proxy()
        args.url = direct.endpoint + '/test'
        before = p.connections
        runner, _ = await self.execute(args, [p])
        self.assertGreater(p.connections, before)
        self.assertEqual(direct.connections, 0)
        self.assertEqual(runner.counts['failed'], 1)

    async def test_auto_mode_measured_local_run(self):
        proxies = [await self.proxy() for _ in range(8)]
        runner, logs = await self.execute(options('--sessions', '8', '--concurrency', 'auto',
                                                '--max-concurrency', '3', '--settle-seconds', '2'), proxies)
        self.assertEqual(runner.counts['ok'], 8)
        self.assertLessEqual(runner.peak_active, 3)
        self.assertGreaterEqual(runner.peak_active, 2)
        print(f'AUTO_METRICS peak_active={runner.peak_active} browser_RSS_peak_MiB={runner.peak_browser_rss/app.MIB:.1f} '
              f'available_min_MiB={runner.minimum_available/app.MIB:.1f} increased={any("Auto admission" in x for x in logs)}')

    async def test_http_proxy_connect_https_keeps_tls_validation(self):
        fixtures = Path(__file__).parent / 'fixtures'
        cert, key = str(fixtures / 'untrusted-cert.pem'), str(fixtures / 'untrusted-key.pem')
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(cert, key)
        async def handler(reader, writer):
            writer.close()
        server = await asyncio.start_server(handler, '127.0.0.1', 0, ssl=tls)
        p = await self.proxy()
        p.tunnel_port = server.sockets[0].getsockname()[1]
        try:
            args = options('--sessions', '1', '--navigation-timeout', '3')
            args.url = 'https://synthetic.test/test'
            runner, _ = await self.execute(args, [p])
            self.assertEqual(runner.errors['tls'], 1)
            self.assertTrue(any(r[1] == 'synthetic.test:443' for r in p.records))
            self.assertTrue(p.endpoint.startswith('http://'))
        finally:
            server.close()
            await server.wait_closed()

    async def test_cli_help_dryrun_missing_url_and_duration(self):
        script = str(Path(app.__file__).parents[1] / 'app.py')
        p = await self.proxy()
        for args, expected in [(['--help'], 0), ([], 2),
                               (['http://synthetic.test/test', '--dry-run'], 0)]:
            proc = await asyncio.create_subprocess_exec(sys.executable, script, *args,
                    stdin=asyncio.subprocess.DEVNULL, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE)
            out, _ = await proc.communicate()
            self.assertEqual(proc.returncode, expected)
            if '--dry-run' in args:
                self.assertEqual(json.loads(out)['network_requests'], 0)
        self.assertEqual(p.connections, 0)
        # Exercise actual main lifecycle, injecting only local candidate source.
        all_proxies = [p] + [await self.proxy() for _ in range(3)]
        args = options('--max-duration', '1', '--settle-seconds', '20')
        timed_runner = app.Runner(args, logger=lambda _: None)
        with patch.object(app, 'load_candidates', return_value=([x.endpoint for x in all_proxies], 'fixture')), \
             patch.object(app, 'preflight'), patch.object(app, 'Runner', return_value=timed_runner):
            start = time.monotonic()
            result = await app.run(args)
        self.assertEqual(result, 0)
        self.assertLess(time.monotonic() - start, 5)
        self.assertEqual(timed_runner.stop_reason, 'max-duration')
        self.assertEqual(timed_runner.active, 0)
        self.assertEqual(surviving_browser_pids(timed_runner), [])
        self.assertLessEqual(sum(len([r for r in x.records if r[1].endswith('/test')]) for x in all_proxies), 2)

    async def test_console_interrupt_real_process_reaps_browser(self):
        p = await self.proxy()
        code = '''import asyncio, sys
sys.path.insert(0, sys.argv[1])
from proxy_session_lab import app
async def fixture(_): return [sys.argv[2]], "local-fixture"
app.load_candidates = fixture
sys.exit(asyncio.run(app.run(app.arguments(["http://synthetic.test/test", "--sessions", "1", "--settle-seconds", "60"]))))
'''
        process_options = {'creationflags': subprocess.CREATE_NEW_PROCESS_GROUP} if os.name == 'nt' else {}
        proc = await asyncio.create_subprocess_exec(sys.executable, '-c', code, str(Path(app.__file__).parents[1]), p.endpoint,
                    stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
                    env={**os.environ, 'PYTHONUNBUFFERED': '1'}, **process_options)
        identities = set()
        try:
            for _ in range(300):
                snap = app.process_snapshot()
                identities.update((pid, snap[pid][1]) for pid in app.descendants(snap, proc.pid))
                if any('/evidence?' in r[1] for r in p.records):
                    break
                await asyncio.sleep(.05)
            self.assertTrue(p.records, 'Child browser did not navigate')
            self.assertTrue(identities)
            proc.send_signal(signal.CTRL_BREAK_EVENT if os.name == 'nt' else signal.SIGINT)
            out, err = await asyncio.wait_for(proc.communicate(), 8)
            self.assertEqual(proc.returncode, 130, (out, err))
            self.assertIn(b'SIGBREAK' if os.name == 'nt' else b'SIGINT', out)
            self.assertIn(b'active=0', out)
            snap = app.process_snapshot()
            self.assertEqual([pid for pid, birth in identities if pid in snap and snap[pid][1] == birth], [])
        finally:
            if proc.returncode is None:
                proc.kill()
                await proc.wait()


if __name__ == '__main__':
    unittest.main(verbosity=2)

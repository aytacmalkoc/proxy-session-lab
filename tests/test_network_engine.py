"""Loopback-only integration for the Go worker and browser readiness regressions."""
import asyncio
from contextlib import suppress
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch
from urllib.parse import urlsplit

from proxy_session_lab import app
from proxy_session_lab.engine import EngineClient, executable
from proxy_session_lab.proxy_source import parse_user_proxies
from test_app import LocalProxy, options, surviving_browser_pids


class ProxyInputTests(unittest.TestCase):
    def test_explicit_protocols_auth_and_redaction(self):
        text = 'http://alice:p%40ss@localhost:8080\nhttps://127.0.0.1:8443\nsocks5://user:pass@proxy.example:1080\nhttp://alice:p%40ss@localhost:8080\n'
        self.assertEqual(len(parse_user_proxies(text)), 3)
        self.assertEqual(app.safe_url('socks5://user:secret@proxy.example:1080'), 'socks5://proxy.example:1080')
        for raw in ('ftp://1.1.1.1:21', 'http://host:0', 'http://host:80/path', 'http://host', 'http://host:80?secret=1'):
            with self.assertRaises(ValueError):
                parse_user_proxies(raw)

    def test_new_arguments_and_network_free_dry_run(self):
        with patch('sys.argv', ['proxy-session-lab','https://fixture.test','--engine','network','--tls-profile','chrome_152','--proxy','http://user:secret@127.0.0.1:8080','--dry-run']), \
             patch('socket.socket', side_effect=AssertionError('network attempted')), \
             patch.object(app, 'run', side_effect=AssertionError('runtime attempted')), \
             patch('sys.stdout', io.StringIO()) as output:
            self.assertEqual(app.main(), 0)
        result = json.loads(output.getvalue())
        self.assertEqual(result['proxy_source'], 'explicit_urls')
        self.assertNotIn('secret', output.getvalue())
        self.assertEqual(result['network_requests'], 0)
        for argv in (['https://fixture.test','--tls-profile','chrome_152'],
                     ['https://fixture.test','--engine','network','--navigation-timeout','61'],
                     ['wss://fixture.test','--engine','network'],
                     ['https://fixture.test','--engine','hybrid','--check','decode']):
            with self.subTest(argv=argv), patch('sys.stderr', io.StringIO()), self.assertRaises(SystemExit):
                app.arguments(argv)


class AsyncInputTests(unittest.IsolatedAsyncioTestCase):
    async def test_explicit_file_and_seed_are_reproducible(self):
        with tempfile.TemporaryDirectory() as td:
            path = Path(td)/'proxies.txt'
            path.write_text('\n'.join(f'http://127.0.0.1:{1000+i}' for i in range(5)))
            args = app.arguments(['http://fixture.test','--engine','network','--proxy-file',str(path),'--seed','42'])
            with patch.object(app, 'load_candidates', side_effect=AssertionError('public source attempted')):
                first, _ = await app.configured_candidates(args)
                second, _ = await app.configured_candidates(args)
            self.assertEqual(first, second)
            args = app.arguments(['http://fixture.test','--direct','--sessions','3'])
            self.assertEqual((await app.configured_candidates(args))[0], ['', '', ''])


class NavigationFixture(LocalProxy):
    async def handle(self, reader, writer):
        task = asyncio.current_task(); self.handlers.add(task)
        try:
            request = await asyncio.wait_for(reader.readuntil(b'\r\n\r\n'), 3)
            path = urlsplit(request.split(b' ')[1].decode()).path
            if path == '/slow.js':
                await reader.read(); return
            status = 403 if path == '/denied' else 200
            body = (b'<html><script src="/slow.js"></script></html>' if self.behavior == 'slow' else
                    b'<html><script>setTimeout(()=>location.href="/denied",50)</script></html>') if path == '/test' else b'denied'
            writer.write(f'HTTP/1.1 {status} Fixture\r\nContent-Type: text/html\r\nContent-Length: {len(body)}\r\nConnection: close\r\n\r\n'.encode()+body)
            await writer.drain()
        except (asyncio.IncompleteReadError, ConnectionError, TimeoutError):
            pass
        finally:
            writer.close()
            with suppress(Exception): await writer.wait_closed()
            self.handlers.discard(task)


class ReadinessRegressionTests(unittest.IsolatedAsyncioTestCase):
    async def scenario(self, behavior):
        p = await NavigationFixture(behavior).start()
        with tempfile.TemporaryDirectory() as td:
            log = Path(td)/'events.jsonl'
            args = options('--sessions','1','--navigation-timeout','.5','--settle-seconds','.25','--log-file',str(log))
            runner = app.Runner(args, logger=lambda _: None)
            try: await runner.execute([p.endpoint], precheck=False)
            finally: runner.events.close(); await p.close()
            events = [json.loads(line) for line in log.read_text().splitlines()]
            return runner, events

    async def test_delayed_script_keeps_received_http_evidence(self):
        runner, events = await self.scenario('slow')
        nav = next(e for e in events if e['event'] == 'navigation')
        session = next(e for e in events if e['event'] == 'session')
        self.assertEqual(runner.counts['failed'], 1)
        self.assertEqual(nav['http_status'], 200)
        self.assertTrue(session['transport_ok'])
        self.assertEqual(session['result'], 'navigation_timeout')

    async def test_late_403_does_not_count_as_success(self):
        runner, events = await self.scenario('redirect')
        session = next(e for e in events if e['event'] == 'session')
        self.assertEqual(session['http_status'], 403)
        self.assertEqual(session['result'], 'target_http_error')
        self.assertEqual(runner.counts['ok'], 0)
        self.assertEqual(runner.counts['failed'], 1)


class NetworkEngineTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        try: self.binary = executable()
        except RuntimeError: self.skipTest('build engine/ to enable real Go integration checks')
        self.proxies = []

    async def asyncTearDown(self):
        for p in self.proxies: await p.close()

    async def proxy(self, behavior='normal'):
        p = await LocalProxy(behavior).start(); self.proxies.append(p); return p

    async def test_two_profiles_persistent_worker_and_no_credential_leak(self):
        p = await self.proxy()
        client = await EngineClient(self.binary, 2).start()
        try:
            reports = await asyncio.gather(*(client.check(url='http://synthetic.test/probe',proxy=p.endpoint.replace('http://','http://alice:secret@'),profile=profile,timeout=2) for profile in ('stdlib','chrome_152')))
            self.assertTrue(all(r['target_ok'] for r in reports))
            self.assertTrue(all(r['reported_egress_ip']=='203.0.113.7' for r in reports))
            self.assertNotIn('secret', json.dumps(reports))
            self.assertEqual(len({r['id'] for r in reports}),2)
        finally: await client.close()
        self.assertIsNotNone(client.process.returncode)

    async def test_network_cli_uses_file_and_never_launches_chromium(self):
        p = await self.proxy()
        with tempfile.TemporaryDirectory() as td:
            source, log = Path(td)/'proxies.txt', Path(td)/'events.jsonl'
            source.write_text(p.endpoint+'\n')
            args = options('--engine','network','--tls-profile','chrome_152','--proxy-file',str(source),'--sessions','1','--log-file',str(log))
            from playwright.async_api import BrowserType
            with patch.object(BrowserType,'launch',side_effect=AssertionError('Chromium launched')):
                code = await app.run(args)
            self.assertEqual(code,0)
            events = [json.loads(line) for line in log.read_text().splitlines()]
            report = next(e for e in events if e['event']=='network_check')
            self.assertTrue(report['transport_ok']);self.assertTrue(report['target_ok'])
            self.assertEqual(report['profile'],'chrome_152')
            self.assertEqual(next(e for e in events if e['event']=='summary')['active'],0)

    async def test_hybrid_runs_browser_only_after_transport_pass(self):
        p = await self.proxy()
        dead = await self.proxy(); endpoint = dead.endpoint
        dead.server.close();await dead.server.wait_closed()
        args = options('--engine','hybrid','--tls-profile','chrome_152','--sessions','2')
        runner = app.Runner(args,logger=lambda _:None)
        try: await asyncio.wait_for(runner.execute([p.endpoint,endpoint],precheck=False),15)
        finally: runner.events.close()
        self.assertEqual(runner.counts['ok'],1);self.assertEqual(runner.counts['failed'],1)
        self.assertEqual(runner.counts['network_passed_before_browser'],1)
        self.assertEqual(runner.counts['transport_checks_failed'],1)
        self.assertEqual(surviving_browser_pids(runner),[])

    async def test_cancelled_engine_request_reaps_process(self):
        p = await self.proxy('hang');client = await EngineClient(self.binary,1).start()
        task = asyncio.create_task(client.check(url='http://synthetic.test',proxy=p.endpoint,profile='stdlib',timeout=10))
        await asyncio.sleep(.1);task.cancel();await asyncio.gather(task,return_exceptions=True)
        await asyncio.wait_for(client.close(),6)
        self.assertIsNotNone(client.process.returncode)

    async def test_hybrid_ready_navigation_precedes_unstarted_network_jobs(self):
        proxies = [await self.proxy() for _ in range(6)]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'events.jsonl'
            args = options('--engine', 'hybrid', '--sessions', '6', '--concurrency', '6',
                           '--max-concurrency', '6', '--navigation-rate', '10',
                           '--settle-seconds', '0', '--log-file', str(path))
            runner = app.Runner(args, logger=lambda _: None)
            try:
                await asyncio.wait_for(runner.execute([p.endpoint for p in proxies], precheck=False), 15)
            finally:
                runner.events.close()
            records = [json.loads(line) for line in path.read_text().splitlines()]
            checks = [r for r in records if r['event'] == 'network_check']
            navigations = [r for r in records if r['event'] == 'navigation']
            self.assertEqual(runner.counts['ok'], 6)
            # A successful precheck must not sit behind the entire first wave.
            first_browser_start = min(r['elapsed_s'] - r['duration_s'] for r in navigations)
            self.assertLess(first_browser_start, max(r['elapsed_s'] for r in checks))
            self.assertEqual({r['session_number'] for r in checks}, set(range(1, 7)))
            self.assertEqual({r['session_number'] for r in navigations}, set(range(1, 7)))
            starts = sorted(r['elapsed_s'] for r in records
                            if r['event'] in ('network_check_started', 'navigation_started'))
            self.assertEqual(len(starts), 12)
            self.assertTrue(all(b-a >= .09 for a, b in zip(starts, starts[1:])), starts)
            self.assertEqual(len([r for r in records if r['event'] == 'context_closed']), 6)
            self.assertEqual(runner.handoff_times.count, 6)
            print('HYBRID_SCHEDULING', json.dumps({
                'first_browser_start_s': first_browser_start,
                'last_network_check_completed_s': max(r['elapsed_s'] for r in checks),
                'minimum_global_start_gap_s': min(b-a for a, b in zip(starts, starts[1:])),
                'handoff': runner.handoff_times.snapshot()}))
            self.assertEqual(surviving_browser_pids(runner), [])

    async def test_go_timeout_updates_total_and_network_stage_counters(self):
        proxy = await self.proxy('hang')
        runner = app.Runner(options('--engine', 'network', '--sessions', '1',
                                    '--navigation-timeout', '.15'), logger=lambda _: None)
        try:
            await asyncio.wait_for(runner.execute([proxy.endpoint], precheck=False), 5)
        finally:
            runner.events.close()
        self.assertEqual(runner.counts['failed'], 1)
        self.assertEqual(runner.counts['timeout'], 1)
        self.assertEqual(runner.counts['network_timeout'], 1)
        self.assertEqual(runner.timeout_stages['http'], 1)


class EvidenceAccountingTests(unittest.IsolatedAsyncioTestCase):
    async def test_partial_http_evidence_does_not_turn_failure_into_success(self):
        report = {'id': 'fixture', 'elapsed_ms': 150, 'transport_ok': False,
                  'target_ok': False, 'http_status': 200, 'http_response_received': True,
                  'body_read_ok': False, 'body_complete': False,
                  'failed_stage': 'body', 'error_code': 'timeout',
                  'stages': [{'name': 'http', 'ok': True},
                             {'name': 'body', 'ok': False, 'code': 'timeout'}]}
        runner = app.Runner(options('--engine', 'network'), logger=lambda _: None)
        runner.network_engine = SimpleNamespace(check=AsyncMock(return_value=report))
        try:
            result = await runner.network_check('http://fixture.test', '', 'target', session_number=3)
            self.assertFalse(result['target_ok'])
            self.assertEqual(runner.counts['timeout'], 1)
            self.assertEqual(runner.counts['http_responses_received'], 1)
            self.assertEqual(runner.counts['body_reads_ok'], 0)
            self.assertEqual(runner.counts['transport_checks_failed'], 1)
            self.assertEqual(runner.network_stages['body']['failed'], 1)
            self.assertEqual(runner.performance()['timeouts_by_stage'], {'body': 1})
        finally:
            runner.events.close()

class ResourcePolicyTests(unittest.TestCase):
    def test_lightweight_network_jobs_do_not_require_browser_memory_reserve(self):
        resources = app.Resources(total=32*1024**3, available=5*1024**3, cpu=0)
        network = app.Controller(app.arguments(['https://fixture.test','--engine','network']))
        browser = app.Controller(app.arguments(['https://fixture.test']))
        self.assertTrue(network.admits(resources))
        self.assertFalse(browser.admits(resources))

    def test_authenticated_socks_is_an_explicit_browser_capability_error(self):
        with self.assertRaises(app.BrowserProxyUnsupported) as caught:
            app.browser_proxy('socks5://user:secret@proxy.example:1080')
        self.assertEqual(app.classify_error(caught.exception),'browser_proxy_unsupported')

class SetupFailureTests(unittest.IsolatedAsyncioTestCase):
    async def test_missing_engine_has_specific_setup_message(self):
        args = options('--engine','network','--engine-path','/definitely/missing/proxy-lab-engine','--direct','--sessions','1')
        logs = []
        runner = app.Runner(args, logger=logs.append)
        try:
            code = await app.run_until_stopped(runner)
        finally:
            runner.events.close()
        self.assertEqual(code,2)
        self.assertTrue(any('engine executable not found' in line for line in logs))
        self.assertEqual(runner.counts['started'],0)

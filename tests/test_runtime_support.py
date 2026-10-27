import asyncio
import ipaddress
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import AsyncMock, Mock, patch

import psutil

from proxy_session_lab import app, runtime_support as runtime


class RuntimeTests(unittest.TestCase):
    def test_resource_monitor_counts_only_our_process_tree_on_each_os(self):
        pid = os.getpid()
        snapshot = {
            pid: (0, 1, 10, 'python'),
            100001: (pid, 2, 20, 'node'),
            100002: (100001, 3, 30, 'Google Chrome Helper (Renderer)'),
            100003: (100001, 4, 40, 'chrome.exe'),
            100004: (100001, 5, 50, 'headless_shell'),
            100005: (0, 6, 1000, 'chrome'),
        }
        with patch.object(runtime, 'process_snapshot', return_value=snapshot), \
             patch.object(psutil, 'virtual_memory', return_value=SimpleNamespace(total=10000, available=7500)), \
             patch.object(psutil, 'cpu_percent', return_value=12.5):
            monitor = runtime.ResourceMonitor()
            measured = monitor.sample()
        self.assertEqual((measured.total, measured.available, measured.cpu), (10000, 7500, 12.5))
        self.assertEqual(measured.app_rss, 10)
        self.assertEqual(measured.helper_rss, 20)
        self.assertEqual(measured.chromium_rss, 120)
        self.assertEqual(measured.chromium_processes, 3)
        self.assertEqual(monitor.browser_identities, {(100002, 3), (100003, 4), (100004, 5)})

    def test_inaccessible_and_exited_processes_do_not_break_monitoring(self):
        inaccessible = Mock(info={'pid': 1, 'ppid': None, 'create_time': None, 'memory_info': None})
        usable = Mock(info={'pid': 2, 'ppid': 1, 'create_time': 10,
                            'memory_info': SimpleNamespace(rss=32), 'name': 'chrome.exe'})
        class Exited:
            @property
            def info(self):
                raise psutil.NoSuchProcess(3)
        with patch.object(psutil, 'process_iter', return_value=[inaccessible, usable, Exited()]):
            self.assertEqual(runtime.process_snapshot(), {2: (1, 10, 32, 'chrome.exe')})

    def test_native_resource_measurement(self):
        resources = runtime.ResourceMonitor().sample()
        self.assertGreater(resources.total, 0)
        self.assertGreaterEqual(resources.available, 0)
        self.assertGreater(resources.app_rss, 0)
        self.assertLessEqual(resources.cpu, 100)

    def test_cache_paths_and_override_do_not_create_directories(self):
        with tempfile.TemporaryDirectory() as directory, \
             patch.object(Path, 'home', return_value=Path(directory)), \
             patch.dict(os.environ, {}, clear=True):
            for platform, suffix in [('linux', '.cache/proxy-session-lab'),
                                      ('darwin', 'Library/Caches/proxy-session-lab'),
                                      ('win32', 'AppData/Local/ProxySessionLab/Cache')]:
                with self.subTest(platform=platform), patch.object(sys, 'platform', platform):
                    result = runtime.cache_directory()
                    self.assertEqual(result, Path(directory) / suffix)
                    self.assertFalse(result.exists())
            for platform, variable in [('linux', 'XDG_CACHE_HOME'), ('win32', 'LOCALAPPDATA')]:
                with patch.object(sys, 'platform', platform), patch.dict(os.environ, {variable: directory}):
                    self.assertTrue(runtime.cache_directory().is_relative_to(directory))
            with patch.dict(os.environ, {'PROXY_SESSION_LAB_CACHE': directory + '/custom'}):
                self.assertEqual(runtime.cache_directory(), Path(directory) / 'custom')

    def test_root_check_when_geteuid_is_absent(self):
        with patch.object(os, 'geteuid', None, create=True):
            self.assertFalse(runtime.is_root())
        with patch.object(os, 'geteuid', return_value=0, create=True):
            self.assertTrue(runtime.is_root())

    def test_signal_handlers_restore_after_success_and_failure(self):
        for fail in (False, True):
            with self.subTest(fail=fail):
                previous = signal.getsignal(signal.SIGINT)
                loop, stop = Mock(), Mock()
                try:
                    with runtime.ShutdownSignals(loop, stop) as handlers:
                        handlers.handle(signal.SIGINT, None)
                        loop.call_soon_threadsafe.assert_called_once_with(stop, 'SIGINT')
                        if fail:
                            raise RuntimeError('fixture')
                except RuntimeError:
                    pass
                self.assertIs(signal.getsignal(signal.SIGINT), previous)

    def test_windows_signal_surface_does_not_require_unix_event_loop(self):
        fake_signal = SimpleNamespace(
            SIGINT=2, SIGTERM=15, SIGBREAK=21,
            getsignal=Mock(return_value='previous'), signal=Mock(),
            Signals=lambda number: SimpleNamespace(name={2: 'SIGINT', 15: 'SIGTERM', 21: 'SIGBREAK'}[number]))
        loop, stop = Mock(), Mock()
        with patch.object(runtime, 'signal', fake_signal):
            with runtime.ShutdownSignals(loop, stop) as handlers:
                self.assertEqual(set(handlers.previous), {2, 15, 21})
                handlers.handle(21, None)
            loop.add_signal_handler.assert_not_called()
            loop.call_soon_threadsafe.assert_called_once_with(stop, 'SIGBREAK')
            self.assertEqual(fake_signal.signal.call_count, 6)

    def test_installed_command_and_module_work_outside_checkout(self):
        command = Path(sys.executable).parent / ('proxy-session-lab.exe' if os.name == 'nt' else 'proxy-session-lab')
        self.assertTrue(command.is_file())
        with tempfile.TemporaryDirectory() as directory:
            for prefix in ([str(command)], [sys.executable, '-m', 'proxy_session_lab']):
                result = subprocess.run([*prefix, 'https://synthetic.test/test', '--dry-run'],
                                        cwd=directory, capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(json.loads(result.stdout)['network_requests'], 0)
                self.assertEqual(list(Path(directory).iterdir()), [])


class AsyncRuntimeTests(unittest.IsolatedAsyncioTestCase):
    async def test_rate_gate_waits_without_turning_delay_into_failure(self):
        gate = app.NavigationGate(100, asyncio.Event())
        await gate.acquire()
        await asyncio.wait_for(gate.acquire(), 1)
        self.assertEqual(gate.starts, 2)

    async def test_diagnostic_body_is_not_read_for_compressed_or_large_responses(self):
        for headers in [
            {'content-length': '20', 'content-type': 'application/json', 'content-encoding': 'gzip'},
            {'content-length': '4097', 'content-type': 'application/json'},
            {'content-type': 'application/json'},
        ]:
            response = SimpleNamespace(headers=headers, body=AsyncMock())
            self.assertIsNone(await app.diagnostic_ip(response, 1))
            response.body.assert_not_awaited()
        response = SimpleNamespace(headers={'content-length': '22', 'content-type': 'application/json'},
                                   body=AsyncMock(return_value=b'{"ip":"203.0.113.7"}'))
        self.assertEqual(await app.diagnostic_ip(response, 1), ipaddress.ip_address('203.0.113.7'))
        response.body.return_value = b'{"ip": true}'
        self.assertIsNone(await app.diagnostic_ip(response, 1))

    async def test_proxy_downloader_timeout_kills_and_reaps_helper(self):
        class Process:
            returncode = None
            communicate = AsyncMock(side_effect=asyncio.TimeoutError)
            kill = Mock()
            wait = AsyncMock()
        process = Process()
        with tempfile.TemporaryDirectory() as directory, \
             patch.object(asyncio, 'create_subprocess_exec', AsyncMock(return_value=process)):
            with self.assertRaises(asyncio.TimeoutError):
                await app.load_candidates(Path(directory))
        process.kill.assert_called_once()
        process.wait.assert_awaited_once()

    async def test_failed_resource_monitor_stops_scheduler(self):
        runner = app.Runner(app.arguments(['https://synthetic.test', '--sessions', '1']))
        async def failed_watch():
            raise OSError('fixture monitor failure')
        async def idle_worker(_):
            await asyncio.Event().wait()
        with patch.object(runner, 'watch', failed_watch), patch.object(runner, 'worker', idle_worker):
            with self.assertRaisesRegex(OSError, 'fixture monitor failure'):
                await asyncio.wait_for(runner.schedule(None, ['http://8.8.8.8:80']), 1)

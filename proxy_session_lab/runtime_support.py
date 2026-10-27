"""Operating-system integration kept outside the browser session engine."""
from dataclasses import dataclass
import os
from pathlib import Path
import signal
import sys

import psutil


@dataclass
class Resources:
    total: int = 0
    available: int = 0
    cpu: float = 0
    app_rss: int = 0
    chromium_rss: int = 0
    helper_rss: int = 0
    chromium_processes: int = 0


def process_snapshot():
    """PID -> (parent, creation time, RSS, name), tolerating process exit races."""
    result = {}
    for process in psutil.process_iter(['pid', 'ppid', 'create_time', 'memory_info', 'name']):
        try:
            info = process.info
            if any(info[key] is None for key in ('ppid', 'create_time', 'memory_info')):
                continue
            result[info['pid']] = (info['ppid'], info['create_time'],
                                   info['memory_info'].rss, info['name'] or '')
        except (psutil.Error, OSError):
            continue
    return result


def descendants(snapshot, root_pid):
    selected = {root_pid}
    while True:
        added = {pid for pid, row in snapshot.items() if row[0] in selected} - selected
        if not added:
            return selected - {root_pid}
        selected.update(added)


class ResourceMonitor:
    def __init__(self):
        self.browser_identities = set()
        psutil.cpu_percent(interval=None)  # Prime the nonblocking delta sampler.

    def sample(self):
        memory = psutil.virtual_memory()
        snapshot = process_snapshot()
        resources = Resources(memory.total, memory.available, psutil.cpu_percent(interval=None))
        resources.app_rss = snapshot.get(os.getpid(), (0, 0, 0, ''))[2]
        for pid in descendants(snapshot, os.getpid()):
            _, birth, rss, name = snapshot[pid]
            if any(part in name.casefold() for part in ('chrome', 'chromium', 'headless_shell')):
                resources.chromium_rss += rss
                resources.chromium_processes += 1
                self.browser_identities.add((pid, birth))
            else:
                resources.helper_rss += rss
        return resources


def cache_directory():
    """Compute only; --help and --dry-run never create user directories."""
    override = os.environ.get('PROXY_SESSION_LAB_CACHE')
    if override:
        return Path(override).expanduser()
    if sys.platform == 'win32':
        base = Path(os.environ.get('LOCALAPPDATA') or Path.home() / 'AppData' / 'Local')
        return base / 'ProxySessionLab' / 'Cache'
    if sys.platform == 'darwin':
        return Path.home() / 'Library' / 'Caches' / 'proxy-session-lab'
    return Path(os.environ.get('XDG_CACHE_HOME') or Path.home() / '.cache') / 'proxy-session-lab'


def is_root():
    get_uid = getattr(os, 'geteuid', None)
    return get_uid is not None and get_uid() == 0


def configure_browser_path():
    """Keep old checkout-local downloads working without overriding user config."""
    local = Path(__file__).resolve().parents[1] / '.browsers'
    if 'PLAYWRIGHT_BROWSERS_PATH' not in os.environ and local.is_dir():
        os.environ['PLAYWRIGHT_BROWSERS_PATH'] = str(local)


class ShutdownSignals:
    """Restore previous handlers; Windows keeps its subprocess-capable event loop."""
    def __init__(self, loop, stop):
        self.loop = loop
        self.stop = stop
        self.previous = {}

    def __enter__(self):
        names = ('SIGINT', 'SIGTERM', 'SIGHUP', 'SIGBREAK')
        try:
            for name in names:
                sig = getattr(signal, name, None)
                if sig is None:
                    continue
                self.previous[sig] = signal.getsignal(sig)
                signal.signal(sig, self.handle)
        except BaseException:
            self.__exit__(None, None, None)
            raise
        return self

    def handle(self, number, _frame):
        self.loop.call_soon_threadsafe(self.stop, signal.Signals(number).name)

    def __exit__(self, *_):
        for sig, previous in self.previous.items():
            signal.signal(sig, previous)
        self.previous.clear()

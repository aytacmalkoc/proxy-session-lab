"""A bounded persistent NDJSON bridge; proxy credentials stay on stdin."""
import asyncio
from contextlib import suppress
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import uuid


class EngineError(RuntimeError):
    pass


def executable(explicit=None):
    candidate = explicit or os.environ.get('PROXY_SESSION_LAB_ENGINE')
    if candidate:
        resolved = shutil.which(candidate)
        if resolved:
            return resolved
        raise EngineError('Network engine executable not found; use --engine-path or build engine/.')
    suffix = '.exe' if os.name == 'nt' else ''
    local = Path(__file__).resolve().parents[1] / 'engine' / 'bin' / ('proxy-lab-engine' + suffix)
    if local.is_file():
        return str(local)
    found = shutil.which('proxy-lab-engine' + suffix)
    if found:
        return found
    raise EngineError('Network engine is not built; see docs/NETWORK_ENGINE.md')


class EngineClient:
    def __init__(self, path=None, workers=8):
        self.path = executable(path)
        self.workers = workers
        self.pending = {}
        self.lock = asyncio.Lock()
        self.reader_task = None
        self.process = None
        self.stderr_task = None

    async def start(self):
        self.process = await asyncio.create_subprocess_exec(
            self.path, 'serve', '--workers', str(self.workers),
            stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE, limit=8 * 1024 * 1024,
            **({'start_new_session': True} if os.name != 'nt' else
               {'creationflags': subprocess.CREATE_NEW_PROCESS_GROUP}))
        # Drain rather than retain arbitrary subprocess diagnostics in memory/logs.
        self.stderr_task = asyncio.create_task(self.drain_stderr())
        try:
            ready = json.loads(await asyncio.wait_for(self.process.stdout.readline(), 3))
            if ready.get('event') != 'ready' or ready.get('protocol') != 1:
                raise ValueError('unsupported protocol')
            self.protocol_info = {key: ready.get(key) for key in ('version','protocol','tls_profiles','tests')}
        except (ValueError, AttributeError, asyncio.TimeoutError):
            await self.close()
            raise EngineError('Network engine protocol mismatch/startup timeout; rebuild engine/') from None
        self.reader_task = asyncio.create_task(self.read())
        return self

    async def drain_stderr(self):
        while await self.process.stderr.read(16384):
            pass

    async def read(self):
        try:
            while line := await self.process.stdout.readline():
                report = json.loads(line)
                if not isinstance(report, dict) or not isinstance(report.get('id'), str):
                    raise EngineError('Invalid engine response')
                future = self.pending.pop(report['id'], None)
                if future is not None and not future.done():
                    future.set_result(report)
        except (ValueError, OSError, EngineError):
            pass
        finally:
            for future in self.pending.values():
                if not future.done():
                    future.set_exception(EngineError('Network engine closed before returning a result'))
            self.pending.clear()

    async def check(self, *, url, proxy, profile, timeout, test='http', decoder='ffmpeg', expect_text=''):
        if self.reader_task.done() or self.process.returncode is not None:
            raise EngineError('Network engine is not running')
        identity = uuid.uuid4().hex
        future = asyncio.get_running_loop().create_future()
        self.pending[identity] = future
        job = {'id': identity, 'url': url, 'proxy': proxy, 'profile': profile,
               'timeout_seconds': timeout, 'test': test, 'decoder': decoder,
               'expect_text': expect_text}
        try:
            async with self.lock:
                self.process.stdin.write(json.dumps(job, allow_nan=False).encode() + b'\n')
                await self.process.stdin.drain()
            return await asyncio.wait_for(future, timeout + 3)
        except (BrokenPipeError, ConnectionResetError, asyncio.TimeoutError) as exc:
            raise EngineError('Network engine did not complete its bounded request') from None
        finally:
            self.pending.pop(identity, None)
            if not future.done():
                future.cancel()

    async def close(self):
        if self.process is None:
            return
        if self.process.returncode is None:
            if self.process.stdin and not self.process.stdin.is_closing():
                try:
                    self.process.stdin.write(b'{"control":"shutdown","id":"shutdown"}\n')
                    await asyncio.wait_for(self.process.stdin.drain(), .5)
                except (OSError, asyncio.TimeoutError):
                    pass
                self.process.stdin.close()
            try:
                await asyncio.wait_for(self.process.wait(), 5)
            except asyncio.TimeoutError:
                if os.name == 'nt':
                    # Restrict forced termination to our own engine process tree.
                    killer = await asyncio.create_subprocess_exec(
                        'taskkill', '/PID', str(self.process.pid), '/T', '/F',
                        stdout=asyncio.subprocess.DEVNULL, stderr=asyncio.subprocess.DEVNULL)
                    try:
                        await asyncio.wait_for(killer.wait(), 3)
                    except asyncio.TimeoutError:
                        killer.kill()
                        await killer.wait()
                else:
                    with suppress(ProcessLookupError):
                        os.killpg(self.process.pid, signal.SIGTERM)
                try:
                    await asyncio.wait_for(self.process.wait(), 2)
                except asyncio.TimeoutError:
                    if os.name != 'nt':
                        with suppress(ProcessLookupError):
                            os.killpg(self.process.pid, signal.SIGKILL)
                    else:
                        with suppress(ProcessLookupError):
                            self.process.kill()
                    await self.process.wait()
        for task in (self.reader_task, self.stderr_task):
            if task is not None and not task.done():
                task.cancel()
        await asyncio.gather(*(t for t in (self.reader_task, self.stderr_task) if t), return_exceptions=True)
        for future in self.pending.values():
            if not future.done():
                future.cancel()
        self.pending.clear()

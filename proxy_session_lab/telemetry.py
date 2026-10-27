"""Bounded performance statistics and optional append-only JSONL events."""
from collections import deque
from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path
import statistics
import time


class Timings:
    def __init__(self, capacity=2048):
        self.recent = deque(maxlen=capacity)
        self.count = 0
        self.total = 0.0
        self.minimum = None
        self.maximum = None

    def observe(self, seconds):
        self.recent.append(seconds)
        self.count += 1
        self.total += seconds
        self.minimum = seconds if self.minimum is None else min(self.minimum, seconds)
        self.maximum = seconds if self.maximum is None else max(self.maximum, seconds)

    def snapshot(self):
        values = sorted(self.recent)
        return {
            'count': self.count,
            'window_count': len(values),
            'mean_s': self.total / self.count if self.count else None,
            'min_s': self.minimum,
            'max_s': self.maximum,
            'median_s': statistics.median(values) if values else None,
            'p95_s': values[math.ceil(.95 * len(values)) - 1] if values else None,
        }


class EventLog:
    def __init__(self, path, run_id):
        self.run_id = run_id
        self.started = time.monotonic()
        self.file = None
        if path:
            destination = Path(path).expanduser()
            if destination.suffix != '.jsonl':
                raise ValueError('--log-file must use the .jsonl extension')
            destination.parent.mkdir(parents=True, exist_ok=True)
            fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
            self.file = os.fdopen(fd, 'a', encoding='utf-8')

    def emit(self, event, **fields):
        if self.file is None:
            return
        record = {
            'timestamp': datetime.now(timezone.utc).isoformat(),
            'elapsed_s': round(time.monotonic() - self.started, 6),
            'run_id': self.run_id,
            'event': event,
            **fields,
        }
        self.file.write(json.dumps(record, ensure_ascii=True, allow_nan=False) + '\n')
        self.file.flush()

    def close(self):
        if self.file is not None:
            self.file.close()
            self.file = None

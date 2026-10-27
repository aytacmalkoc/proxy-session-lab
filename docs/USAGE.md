# Usage guide

Run these examples from the project directory with a target you own. The
existing installation is at `~/proxy-browser-test`; a fresh clone normally uses
the `proxy-session-lab` directory.

## Understand the limits

```bash
./run.sh "https://your-app.example/test" \
  --sessions 100 --concurrency 4 --max-concurrency 4 \
  --navigation-rate 1 --max-duration 600
```

This allows up to 100 attempts, 4 active session slots, and one application-started
main navigation per second. A slot is replaced when it finishes. Failures consume
the budget. The run stops at 600 seconds even if fewer than 100 attempts finished.

Active slots include context setup and navigation-rate waiting. Four active
contexts do not necessarily mean four successfully loaded pages.

## Keep loaded pages open

`--settle-seconds` waits after DOMContentLoaded. It does not refresh the page or
repeat a visit. The page's own background activity can continue during this wait.

```bash
# Keep each successfully loaded target page open for 10 minutes.
./run.sh "https://your-app.example/test" \
  --sessions 4 --concurrency 4 --max-concurrency 4 \
  --navigation-rate 1 --settle-seconds 600 --max-duration 900
```

### Plan the deadline

If there are enough slots for all attempts and no probe, a planning estimate is:

```text
(sessions - 1) / navigation_rate
  + navigation_timeout + settle_seconds
  + preparation / resource-wait margin
```

For 100 slots and attempts, rate 1, navigation timeout 30, and settle delay 600,
this is `99 + 30 + 600 = 729 seconds` before preparation and other delays.
A 900-second deadline adds margin; it does not guarantee completion.

When the concurrency limit is smaller, later attempts must also wait for earlier
slots to finish. A conservative planning estimate is:

```text
ceil(sessions / concurrency) * (navigation_timeout + settle_seconds)
  + (sessions - 1) / navigation_rate
  + preparation / resource-wait margin
```

This deliberately overestimates overlapping work. Probe mode adds another
navigation per attempt, and resource admission can delay work further. Read the
final counters instead of assuming the requested budget completed.

## Request a 100-slot ceiling

```bash
./run.sh "https://your-app.example/test" \
  --sessions 100 --concurrency 100 --max-concurrency 100 \
  --navigation-rate 1 --navigation-timeout 30 \
  --settle-seconds 600 --max-duration 900
```

The code accepts these limits, but 100 concurrent sessions have not been
validated on the installation machine. Resources and proxy failures can prevent
100 successful pages from overlapping. Increasing concurrency does not repair
unreliable proxies. Increase limits using measurements from your own target.

## Proxy diagnostics

```bash
./run.sh "https://your-app.example/test" \
  --sessions 10 --concurrency 2 --max-concurrency 2 \
  --probe-url "https://your-diagnostics.example/egress" \
  --navigation-rate 1 --max-duration 180
```

For each attempt, the same context/proxy/Page opens the probe, clears cookies and
storage at the configured probe origin, and then opens the target. Probe and
target share the rate gate. A failed probe consumes the attempt and skips the
target. Main HTTP 429/503 stops the entire run.

To report egress IP, your endpoint must return a successful response with:

- A JSON Content-Type.
- A numeric Content-Length of at most 4096 bytes.
- A JSON object with a parseable IP string, such as `{"ip":"203.0.113.7"}`.

The output is labeled `diagnostic_reported_egress_ip`: it is the endpoint's
report. Other successful probe responses allow the target attempt without
verifying an IP. Proxy assignment alone does not verify egress. Use an endpoint
you own. Without this option, no separate proxy-validation site is contacted.

## Interpreting failures

| Category | Meaning / next check |
| --- | --- |
| `proxy_connection` | Proxy or CONNECT tunnel error; inspect the printed `ERR_*` code |
| `dns` | Name-resolution error; proxy-side DNS can differ from local DNS |
| `tls` | Certificate/TLS error; certificate validation remains enabled |
| `navigation_timeout` | Navigation or network timeout; this alone does not establish the cause |
| `network_navigation_error` | Other network/navigation error; inspect the per-session Chromium error code |
| `target_http_error` | Main target HTTP response was 400 or higher |
| `probe_http_error` | Optional probe did not return a usable successful main response |
| `no_main_response` | Target navigation returned no main response |
| `browser_context_crash` | Browser/page/context unexpectedly closed or crashed |
| `resource_exhaustion` | Explicit memory/resource-exhaustion evidence |

Unknown failures are not labeled as target blocks. Check `errors={...}`,
`reason=...`, per-session `result=...`, and HTTP status. Proxy errors alongside
abundant RAM do not establish a memory shortage. An absence of target HTTP
errors does not prove every failed request reached the target.

Free-list membership does not guarantee a working connection to your target.
Increasing attempts, concurrency, or timeouts does not by itself prove or fix
the cause. No failed candidate is assigned to another attempt within the run.

### Counters that are easy to misread

- `planned`: requested attempts, not visits that reached the target.
- `started`: includes setup failures before navigation.
- `navigations`: rate-gate grants for probe/target `page.goto`, not redirects or
  subresource requests.
- `succeeded`: target navigation and settling finished, not a counter increment.
- `timeout`: already included in `failed`.
- `cancelled`: work interrupted by the deadline or shutdown.
- `peak_active`: peak session slots, not fully loaded pages.

After the shutdown regression fix, cancellation during context close waits for
the bounded cleanup and still updates `active` and `finished`. Repeated shutdown
signals do not abandon that in-flight cleanup. A successful cleanup finishes
with `active=0`.

## Performance and JSONL logs

```bash
./run.sh "https://your-app.example/test" \
  --sessions 100 --concurrency auto --max-concurrency 8 \
  --navigation-rate 1 --max-duration 600 \
  --log-file logs/run.jsonl
```

The console keeps its status line and adds a `PERF` line approximately every
five seconds and at shutdown:

```text
[run_id] PERF elapsed_s=30.0 success=75.0% finished_per_s=0.40 queue=0 nav_median_s=1.20 nav_p95_s=4.80 nav_window=14 CPU_peak=45% RSS_app_peak~48MiB RSS_Chromium_peak~620MiB
```

This is an illustrative line, not a benchmark. Definitions:

| Metric | Definition |
| --- | --- |
| `success` | Successful / (successful + failed), excluding cancellations; `-` before any resolved outcomes |
| `finished_per_s` | Finished sessions / elapsed run seconds, including failed and cancelled sessions |
| `queue` | Jobs waiting in the bounded work queue, not all unstarted planned attempts |
| `nav_median_s`, `nav_p95_s` | Median and nearest-rank p95 of up to the last 2048 navigation elapsed times |
| `nav_window` | Sample count used by those recent navigation percentiles |
| `CPU_peak` | Highest sampled system-wide CPU utilization, including other applications |
| `RSS_*_peak` | Peak sampled RSS sums, with the usual shared-memory caveat |

Navigation timing starts after the rate gate and ends when `page.goto` returns,
fails, or is cancelled. It includes probes and target navigations but excludes
the rate wait, context setup, settle delay, and cleanup. Cancelled navigation
samples are truncated at cancellation, not full successful-load times. Per-session
elapsed time includes all session phases and cleanup.

The JSONL file is append-only. Reusing a path keeps earlier runs, separated by
`run_id`. Each line contains a UTC `timestamp`, `elapsed_s`, `run_id`, and `event`:

- `run_started`: configuration and target.
- `status`: counters, active limit, and sampled resources (raw memory in bytes).
- `performance`: aggregate timings, throughput, success percentage, and peaks.
- `navigation`: probe/target role, elapsed seconds, and HTTP status if available.
- `session`: proxy, result, last work phase, HTTP status, Chromium error code,
  navigation time when available, total session time, and active count.
- `summary`: final counts, stop reason, error categories, `network_errors`,
  and all performance aggregates.

Timing snapshots keep a lifetime count/mean/min/max, with bounded recent samples
for median/p95. The file is flushed after each event. New files use mode `0600`,
missing parent directories are created, and `logs/` plus `*.jsonl` are excluded
from Git. Existing file permissions are left unchanged. No HTML bodies,
subresource dumps, or raw exception call logs are written. Keep these files local
if your target URL or configuration is sensitive. There is no automatic log
rotation; use a fresh path for each experiment if desired.

Dry-run shows the requested path but does not create it. Without `--log-file`,
performance remains visible in the console and no event file is written.

Print only final summaries from a log using the standard library:

```bash
.venv/bin/python - <<'PY'
import json
from pathlib import Path
for line in Path('logs/run.jsonl').read_text().splitlines():
    record = json.loads(line)
    if record['event'] == 'summary':
        print(json.dumps(record, indent=2))
PY
```

## Local, private, and Tailscale targets

A public proxy may not reach an address available only on your LAN or tailnet.
Preflight warns about recognized local/private/CGNAT/Tailscale names or addresses;
it is a heuristic, not a reachability guarantee. Firewall/router settings are
not changed. Chromium's implicit loopback proxy bypass is disabled, so localhost
targets do not silently use direct access.

Production proxy parsing rejects private endpoints. Tests inject loopback
fixtures through an internal Python seam, not a public CLI option.

## Stop a run

Press Ctrl+C once and wait for cleanup and the shell prompt. New work stops;
active and pending jobs are cancelled. The enclosing browser is closed before
the summary. SIGTERM and SIGHUP also request shutdown. No automatic service is
installed and the app does not restart itself.

## Installation troubleshooting

- Use `--help` and `--dry-run` first. They do not launch Chromium; dry-run also
  does not access the network.
- Create `.venv` and install `requirements-lock.txt` if the venv is missing.
- Install Chromium with `PLAYWRIGHT_BROWSERS_PATH="$PWD/.browsers"` if missing.
- Detect the distribution and use `ldd` for missing shared libraries. Resolve
  package names for your distribution, especially on Arch/CachyOS.
- Use a non-root account. Restricted execution containers can block sandbox
  system calls that work in a normal terminal; the app keeps sandboxing enabled.
- `--headed` requires a desktop session; automated tests cover headless mode.

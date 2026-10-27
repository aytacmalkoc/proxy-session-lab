# Proxy Session Lab

A terminal-only tool for controlled, finite synthetic browser sessions against
a web application you own. Built with Python, `asyncio`, Playwright, and Chromium,
using HTTP proxy candidates from Proxifly's free list.

**One Chromium browser. Isolated contexts. Bounded concurrency. A separate
navigation rate limit. No web UI or results database.**

Successful navigation does not prove a page-view counter increment, a verified
egress IP, or a visit by a real person or physical device. Observe your counter
in your own application.

## Quick start

On the machine where this project is already installed:

```bash
cd ~/proxy-browser-test
./run.sh "https://your-app.example/test"
```

Preview configuration without any network requests, DNS lookups, or browser:

```bash
./run.sh "https://your-app.example/test" --dry-run
```

Omit the URL to enter it interactively. A missing URL in non-interactive mode is
an argument error. Press **Ctrl+C** to stop admission, cancel pending work, and
close Chromium. Allow a short cleanup period before the shell prompt returns.

## Installation

Use Python **3.11+** and a dedicated virtual environment. Runtime dependencies
are Playwright and psutil, plus their transitive dependencies. Chromium is
installed separately for the current operating system and CPU architecture.

### macOS / Linux

```bash
git clone https://github.com/aytacmalkoc/proxy-session-lab.git
cd proxy-session-lab
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements-lock.txt
.venv/bin/python -m pip install --no-deps -e .
.venv/bin/python -m playwright install chromium
./run.sh --help
```

### Windows (PowerShell)

```powershell
git clone https://github.com/aytacmalkoc/proxy-session-lab.git
cd proxy-session-lab
py -3 -m venv .venv
.\.venv\Scripts\python.exe -m pip install -r requirements-lock.txt
.\.venv\Scripts\python.exe -m pip install --no-deps -e .
.\.venv\Scripts\python.exe -m playwright install chromium
.\.venv\Scripts\proxy-session-lab.exe --help
.\.venv\Scripts\proxy-session-lab.exe "https://your-app.example/test"
```

`run.sh` (macOS/Linux) and `run.ps1` (Windows) use the checkout's own virtual
environment. The Windows executable above also works without running a
PowerShell script. After activating the venv, every OS supports:

```text
proxy-session-lab "https://your-app.example/test"
python -m proxy_session_lab "https://your-app.example/test"
```

The installed command works from any working directory. Playwright normally
uses its OS-specific browser cache. Existing checkout-local `.browsers` folders
are detected automatically; an explicit `PLAYWRIGHT_BROWSERS_PATH` takes priority.
Old `python app.py ...` invocations remain supported. Install this update's
requirements before using an existing `~/proxy-browser-test` checkout.

Use operating-system versions supported by the pinned Playwright release;
portability does not add support for obsolete Windows/macOS versions. Native
release executables with a bundled Python interpreter are not included.

### Linux libraries

Detect the actual distribution before installing system packages. On
CachyOS/Arch, do not blindly run Ubuntu's `apt` commands or `install-deps`.
Use `ldd` on the downloaded Chromium executable to find missing shared libraries,
then resolve them using distribution-appropriate packages.

The local CachyOS installation already had the required libraries. Both full
Chromium and its headless shell passed the library check; sandboxed headless
Chromium launched successfully. Playwright reported an unsupported OS and
downloaded an Ubuntu-compatible build. No system package changes were needed.
See [verification notes](docs/VERIFICATION.md).

## Options

All times are in **seconds**. Counts must be positive integers. Rates/timeouts
must be finite positive numbers; the settle delay can be zero.

| Option | Default | Meaning |
| --- | --- | --- |
| `URL` | prompt | Your HTTP(S) target; embedded credentials are rejected |
| `--sessions` | `20` | Total attempts, including failures and failed probes |
| `--concurrency` | `auto` | Active session limit: a number or `auto` |
| `--max-concurrency` | `8` | Absolute active-context/admission ceiling |
| `--navigation-rate` | `1` | Application-started main navigations per second |
| `--max-duration` | `120` | Overall deadline, including preparation |
| `--navigation-timeout` | `30` | Timeout for each probe or target navigation |
| `--settle-seconds` | `5` | Wait after target DOMContentLoaded |
| `--headed` | off | Show Chromium; requires a desktop session |
| `--probe-url` | none | Optional diagnostics endpoint that you own |
| `--dry-run` | off | Print configuration without network or browser access |
| `--log-file` | none | Append structured events and performance data to a `.jsonl` file |

Explicit concurrency must not exceed `--max-concurrency`. The effective limit
is also bounded by the total session budget.

## Sessions versus concurrency versus rate

- **Sessions** is the total attempt budget.
- **Concurrency** is the number of active session slots. Slots can include
  context setup, waiting for the rate gate, navigation, settling, and cleanup.
  They do not all represent successfully loaded pages.
- **Navigation rate** controls how frequently the application starts `page.goto`.
  Probe and target navigations share this gate.

With 100 sessions and concurrency 8, a rolling pool replaces each completed
slot with the next attempt. The program does not wait for a whole fixed batch
to finish. At rate 1, application-started main navigations still begin at most
once per second.

The deadline cancels active work even if settling is unfinished. If all attempts
finish first, the program exits early. It also exits if distinct proxy candidates
are exhausted. Cleanup can take a short additional period after the deadline.

### Conservative first run

```bash
./run.sh "https://your-app.example/test" \
  --sessions 20 --concurrency 2 --max-concurrency 2 \
  --navigation-rate 1 --max-duration 120
```

### 100 attempts with automatic concurrency

```bash
./run.sh "https://your-app.example/test" \
  --sessions 100 --concurrency auto --max-concurrency 8 \
  --navigation-rate 1 --max-duration 600
```

For long-lived pages, duration planning, diagnostics, and higher limits, see
the [usage guide](docs/USAGE.md).

## Resource control

Auto mode starts at 2 slots, or fewer when the configured budget requires it.
It samples CPU, available RAM, and process memory through psutil and may
increase the limit by one every five seconds when current slots are occupied
and resources permit.

Approximately 25% of total RAM is reserved, with an extra 128 MiB admission
margin. Sustained high CPU (three samples at 85% or higher), low RAM, or an
explicit resource-exhaustion error reduces or delays admission. Active sessions
are not killed merely to reduce this limit. Fixed concurrency also observes
resource admission checks. Proxy timeouts are not automatically counted as
resource exhaustion.

Auto mode uses local resource evidence. It does not establish proxy reliability
or available capacity on the target.

## Browser and proxy behavior

- One browser per run; one nonpersistent context, one Page, and one assigned
  proxy per attempt. Cookies/localStorage are isolated between attempts.
  Normal Chromium child processes are expected.
- A bounded worker pool and queue avoid creating one pending task per attempt.
- The HTTP list is downloaded once when no fresh user cache exists. Cache lifetime
  is 5 minutes, maximum download size is 2 MiB, and download time is bounded.
  TLS is verified; the downloader does not use environment proxies.
- Candidates must be literal global unicast IPs with valid ports and `http://`
  endpoints. Duplicates, credentials, hostnames, private/local/CGNAT/multicast
  addresses, and malformed records are rejected.
- Each candidate is assigned at most once per run. Failures consume the session
  budget. There is no application retry loop; Chromium can make its own
  underlying connection attempts.
- HTTP proxies can tunnel HTTPS targets using CONNECT. A target's HTTPS scheme
  does not change the proxy endpoint from `http://` to `https://`.
- Target TLS validation stays enabled. Unix root execution is rejected and Chromium
  sandboxing is explicitly enabled.
- JavaScript runs normally. No browsing interactions, link following, form
  submissions, reload loops, resource blocking, or injected scripts are added.
  Readiness uses DOMContentLoaded and a finite settle delay, not network idle.
- Video, screenshots, trace, and HAR recording are disabled.
- A main response of **429 or 503** stops the run, including probes and redirects.
  The program does not rotate proxies to continue past that response.
- Without `--probe-url`, the first target navigation is the attempt; no other
  site is automatically used for validation. Proxy assignment alone does not
  verify egress IP. See [probe requirements](docs/USAGE.md#proxy-diagnostics).
- Local/private/Tailscale targets receive a preflight warning. Chromium's
  implicit loopback proxy bypass is disabled. No firewall/router settings or
  automatic services are changed.

## Writable user data

The proxy cache is independent of the installation directory:

| OS | Default cache directory |
| --- | --- |
| Linux | `$XDG_CACHE_HOME/proxy-session-lab`, or `~/.cache/proxy-session-lab` |
| macOS | `~/Library/Caches/proxy-session-lab` |
| Windows | `%LOCALAPPDATA%\ProxySessionLab\Cache` |

`PROXY_SESSION_LAB_CACHE` overrides this directory. Help and dry-run do not
create it. Logging remains opt-in: `--log-file` uses exactly the path supplied,
with relative paths resolved from the current working directory. New cache/log
files request Unix mode 0600; on Windows, access follows the containing folder's
Windows permissions rather than Unix mode bits.

## Synthetic traffic identification

Each run has a UUID `run_id`, shown in console events. `X-Synthetic-Test-Run`
is attached only to top-level document requests at the configured target origin
(scheme, host, and port). It is not added to iframe documents, subresources, or
unrelated origins, including cross-origin redirects. This uses per-request CDP
overrides rather than a global context header; resources are not blocked.

## Reading output

```text
[run_id] STATUS planned=20 started=8 active=2 finished=6 succeeded=2 failed=4 timeout=1 cancelled=0 limit=2 navigations=8 RAM_available=24000MiB CPU=35% RSS_app~46MiB RSS_Chromium~450MiB RSS_helpers~100MiB Chromium_processes=7
[run_id] session=4 proxy=http://<proxy-ip>:<port> HTTP=200 result=navigation_succeeded navigation_s=1.25 session_s=6.40 active=1
```

`timeout` is a **subset of `failed`**, not an additional failure count.
`cancelled` means interrupted work, for example at the deadline. `finished`
advances after session cleanup; outcome counters can advance slightly earlier.

Summed RSS can count shared memory more than once. It is an approximation, not
exact physical RAM usage. The final output reports the stop reason, elapsed
time, peak active slots, peak Chromium RSS, minimum available RAM, and errors.
See [troubleshooting](docs/USAGE.md#interpreting-failures).

Each status update also includes a compact `PERF` line: elapsed time, success
percentage, completed sessions per second, queue depth, navigation median/p95,
and sampled peak CPU/RSS. Navigation timings exclude the rate-gate wait and
include probes, successes, failures, and interrupted navigations. Median/p95 use
the latest 2048 samples; mean/min/max in JSONL span the full run. CPU peaks are
peaks of sampled system-wide utilization, not continuous per-process profiling.

### Save structured performance data

```bash
./run.sh "https://your-app.example/test" \
  --sessions 100 --max-duration 600 --log-file logs/run.jsonl
```

This preserves readable console output and appends UTC-timestamped JSON records
with a run ID. Records include `run_started`, `status`, `performance`,
`navigation`, `session`, and `summary`, plus general messages. Each final summary
contains error categories, Chromium `ERR_*` counts, counts, and performance data.
There is no database. Logging is off by default; dry-run creates no file.
See the [logging guide](docs/USAGE.md#performance-and-jsonl-logs).

| Exit code | Meaning |
| --- | --- |
| `0` | Budget/candidates completed or deadline reached without recorded session failures; deadline cancellations can still exist |
| `1` | One or more session failures |
| `2` | Argument/setup/runtime error or protective stop such as HTTP 429/503 |
| `130` | SIGINT / Ctrl+C, or Windows SIGBREAK / Ctrl+Break |
| `143` | SIGTERM; currently also used for SIGHUP |

## Tests and limitations

```bash
PLAYWRIGHT_BROWSERS_PATH="$PWD/.browsers" \
  .venv/bin/python -m unittest discover -s tests -v
```

Tests use loopback fixtures and real sandboxed Chromium. They do not select a
third-party target. The untrusted TLS certificate/key are public, test-only fixtures;
no external `openssl` executable is required. Unix interrupt tests use SIGINT;
Windows uses CTRL_BREAK_EVENT against a separate process group. Both exercise
the real child process and verify browser cleanup.

See [verification notes](docs/VERIFICATION.md) for checks, resource observations,
and the shutdown regression fix. Public-proxy reliability,
counter increments, headed mode, and 100-context capacity have not been validated
by the developer's automated checks. Start at concurrency 2 and rate 1, then
measure your actual target.

## Sources

- [Proxifly repository](https://github.com/proxifly/free-proxy-list)
- [HTTP list used by this application](https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/protocols/http/data.txt)
- [Playwright context proxy API](https://playwright.dev/python/docs/api/class-browser#browser-new-context)
- [Chromium launch and sandbox options](https://playwright.dev/python/docs/api/class-browsertype#browser-type-launch)

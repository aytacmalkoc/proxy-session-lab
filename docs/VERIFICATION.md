# Verification notes

## Cross-platform update

The application now targets Python 3.11+ on Linux, macOS, and Windows.
`psutil==7.2.2` replaces Linux `/proc` reads. A dedicated package and console
entry point work independently of the current working directory; user cache
locations no longer depend on a writable source directory. The subprocess
download deadline replaces Unix-only SIGALRM. Signal handlers are restored on
exit and Windows supports Ctrl+Break in addition to Ctrl+C.

The development macOS session blocks process enumeration through `sysctl`.
Its resource-independent checks passed with an explicitly mocked resource
monitor; this is not a native resource-measurement or browser acceptance result.
Native Windows/macOS acceptance and a Python-version matrix have not been
validated. No CI workflow is included. Public proxy endpoints and view counters
are not part of the automated suite.

Additional regression checks cover platform cache locations, Windows signal
handling without Unix event-loop methods, parent-enforced downloader timeout
and reaping, process naming across platforms, inaccessible/exited processes,
the rate-gate delay, and skipping compressed or oversized diagnostic JSON.
The TLS rejection test uses a committed, deliberately public test-only key and
self-signed certificate; it no longer needs an external `openssl` command.

### Results on 2026-10-27

- Full suite: **30 tests passed in 17.292 seconds** on Ubuntu 24.04 arm64,
  Python 3.12.3, in the official Playwright 1.64.1 Docker image. The container
  ran as `pwuser`, with an init process to reap orphaned child processes and
  permissions for Chromium's sandbox namespace syscalls. Chromium sandboxing
  remained enabled. Real navigation, proxy isolation, TLS rejection, resource
  sampling, and interrupt cleanup were included.
- macOS Python 3.12.14: 18 resource-independent checks passed with an explicitly
  mocked monitor, plus compile, help, network-free dry-run, dependency, and wheel
  packaging checks. Native process enumeration is blocked by the development
  session's sandbox, so native browser acceptance is not claimed.
- The parent-managed Proxifly downloader completed with TLS verification and
  parsed 5778 candidates. A second read used the cache and returned the same
  set. These counts are a snapshot; proxy reachability was not tested.
- The installed CLI and `python -m proxy_session_lab` were both exercised from
  a temporary directory outside the source checkout.
- Native Windows acceptance remains unverified. Windows path/signal behavior
  is covered by isolated regression tests, and the console-interrupt browser
  test includes a Windows CTRL_BREAK_EVENT path for future native execution.

## Original Linux environment

The local installation was checked on 2026-10-27:

| Component | Version |
| --- | --- |
| Distribution | CachyOS, Arch family |
| Python | 3.14.8 |
| pip | 26.2.2 |
| Playwright | 1.64.1 |
| Chromium | 156.0.8078.4, Playwright revision 1248 |

Python packages are pinned in [`requirements-lock.txt`](../requirements-lock.txt).
The app has its own venv and browser downloads, excluded from Git. Other project
environments were not changed. No `apt`, `install-deps`, firewall, router, or
automatic service changes were made.

`ldd` found no missing libraries for full Chromium or the headless shell.
A real browser launched with `chromium_sandbox=True`.

## Original Linux automated checks

The acceptance suite uses loopback HTTP proxy/HTML/TLS fixtures and real sandboxed
Chromium. It covers:

- Untrusted proxy parsing and duplicate/private-address rejection.
- CLI validation, help, missing URL, and network-free dry-run.
- One browser launch per run and per-context proxy assignment.
- Cookie/localStorage isolation and JavaScript execution.
- Synthetic header scope, including cross-origin redirects.
- Session, context, rate, and overall duration limits; cleanup.
- Auto admission decisions and measured local execution.
- Proxy failure and timeout budgets without candidate reuse.
- HTTP 429/503 stopping without continuing on another proxy.
- HTTP CONNECT for HTTPS while rejecting an untrusted certificate.
- Disabling implicit localhost proxy bypass.
- SIGINT sent to a real child process and browser/driver cleanup afterward.

The local TLS rejection fixture can print an expected server-side certificate
alert. The passing assertion checks that the browser rejected the certificate.
This original suite generated a certificate using the machine's existing
`openssl`. The current suite uses the public fixture described above.

The first English publication passed 13 of 14 tests in 17.808 seconds; the
HTTP 429/503 test reproduced the cleanup-accounting race (`active=1`). The
follow-up fix adds deterministic repeated-cancellation regression coverage,
bounded timing-statistics checks, JSONL append/UTC/permissions checks, and a
real-browser structured-log test. Dry-run also checks that no log file is created.
Tests do not select any third-party target or validation service.

The follow-up suite passed **18 tests in 18.862 seconds**, including the
previously failing HTTP 429/503 path and both deterministic cancellation timings.

## Original download check

During initial installation, the official HTTP list was downloaded with TLS
validation and yielded 5606 valid candidates. A second call read the same set
from the five-minute cache. This was a point-in-time data-source check, not a
test of the listed proxy endpoints. The source and count change over time.

## Resource observations and limits

Lightweight local pages used approximately 435-445 MiB summed Chromium RSS with
2 active slots and 629-634 MiB with 3. Auto increased from 2 to 3 during a local
run. Shared memory makes these approximations, not physical-memory totals.

They do not predict an arbitrary target's capacity. The developer did not run
a public-proxy end-to-end check, a 100-context capacity benchmark, or headed-mode
acceptance. Start with concurrency 2 and rate 1, then measure your own target.
Navigation success does not verify a counter increment.

## Shutdown regression fix

Previously, cancellation during an already-running context cleanup could skip
the final counter updates. Final `active` could remain nonzero and `finished`
could lag after Chromium had exited. A context-close fixture reproduced this
before the fix, both for a completed session and one cancelled during work.

Cleanup now runs as a shielded bounded task. Repeated cancellation is remembered
and propagated after cleanup; bookkeeping runs in a separate `finally` block.
The regression checks actual context close completion, empty context tracking,
`active=0`, exactly one finished count, and correct outcome accounting in both
timings. The close timeout and enclosing browser shutdown remain in place.

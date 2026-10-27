# Verification notes

## Environment

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

## Automated checks

The 14-test suite uses loopback HTTP proxy/HTML/TLS fixtures and real sandboxed
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
Certificate generation uses the machine's existing `openssl`; the runtime does
not require that command.

After localization, 13 of 14 tests passed in 17.808 seconds. The HTTP 429/503
test reproduced the known cleanup-accounting race (`active=1` after shutdown).
This first publication preserves that known issue for a separate fix. Tests
do not select any third-party target or validation service.

## Separate download check

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

## Known limitation

If cancellation arrives during an already-running context cleanup, it can skip
that cleanup's final counter updates. Final `active` can remain nonzero and
`finished` can lag even after the enclosing browser shutdown removed its
processes. This was reproduced without network traffic using a context-close
fixture.

Normal deadline/SIGINT acceptance cases pass but do not cover every cancellation
timing. English localization preserves the existing lifecycle and does not fix
this edge case. Read the process count and stop reason alongside counters when
it occurs.

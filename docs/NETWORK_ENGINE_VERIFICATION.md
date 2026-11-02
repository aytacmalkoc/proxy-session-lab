# Network integration verification — 2 November 2026

Branch: `codex/proxy-network-engine`, based on proxy-session-lab `dcce328`.
The imported kickpop source is identified in [engine provenance](../engine/PROVENANCE.md).

## Linux scheduling and evidence fixes — 2 November 2026

The follow-up local build on CachyOS/Python 3.14.8/Go 1.27.3 adds ready-browser
priority within the single global rate gate, per-stage timeout accounting,
HTTP-response/body evidence, session-correlated lifecycle events, and a redacted
local interface configuration digest. No dependencies or target-success criteria
were relaxed.

Before the fixes, local regressions failed for browser handoff order, Go timeout
counting, and HTTP evidence after a body deadline. After the fixes:

- The full Python suite passed **47 tests**. The six-session hybrid fixture
  began browser navigation at 0.318s while the last Go check finished at 1.227s;
  starts remained at least 0.100s apart with a configured 10/s global rate.
  Observed handoff p95 was 0.099s in this lightweight local fixture. These are
  scheduling observations, not remote performance/capacity predictions.
- Go `go test -race ./...` passed (178 test/subtest pass events, four opt-in
  capture/load/offline-input cases skipped); `go vet ./...` and the native engine
  build passed. Synthetic FFmpeg proxy/decode checks were enabled locally.
- Regressions cover priority/FIFO order, cancellation and stop cleanup, preserved
  global rate, Go timeout totals, body-timeout evidence without false success,
  bounded-body completion semantics, session correlation, and network-digest
  stability/redaction/unavailable handling.

No new public-proxy, Kick or third-party test traffic was sent. The source of the
previous simultaneous `ERR_NETWORK_CHANGED` events is still unproven; the added
diagnostics make that question observable rather than suppressing/retrying errors.

## Completed checks

- Python: **42 tests passed in 23.052 seconds** on native macOS arm64, Python
  3.12.14, using the pinned real Playwright/Chromium installation and real local
  proxy sockets. Network bridge, hybrid filtering, cancellation, credential
  redaction, explicit source/seed, missing-engine diagnostics, and both browser
  readiness regressions are included.
- The small browser fixtures explicitly select **10% RAM reserve**. The host's
  available RAM was below the application's normal 25% browser reserve during an
  earlier run, so admission correctly waited and those tests timed out. The CLI
  now exposes `--ram-reserve-percent`; production defaults remain 25% for browser/
  media/decode and 10% for lightweight network work. Native resource samples were
  retained; the passing run did not mock the resource monitor.
- Go: `go test -race ./...` and `go vet ./...` passed, with local FFmpeg integration
  enabled. Follow-up focused race tests also passed for WSS library-version
  metadata and the real ClientHello-offer comparison.
- Native Go engine build passed. Windows amd64 and Linux amd64 cross-builds
  passed with `CGO_ENABLED=0`. This is not native execution/acceptance on those OSes.
- New documentation links, Python syntax and Git whitespace checks passed.

## Transport and capability coverage

| Scenario | Verified result |
| --- | --- |
| `stdlib` / `chrome_152` through authenticated HTTP CONNECT | Verified origin TLS + negotiated HTTP/2 |
| Actual ClientHello comparison at a local TLS origin | The two profiles sent different cipher offers |
| Invalid/untrusted origin certificate | Explicit TLS rejection; no verification bypass |
| HTTPS proxy | Separate verified proxy TLS, CONNECT, then origin TLS |
| SOCKS5 username/password | Authentication and domain-name forwarding to the proxy |
| Proxy 407 / target 403 / target 503 | Proxy authentication and target policy classified separately; target 503 stops the run |
| Header/body bounds | Oversized HTTP/1 response header rejected; HTTP body limited to 64 KiB + sentinel |
| Dead proxy / stalled CONNECT | Correct failed stage and bounded cancellation |
| Go worker bridge | Protocol/version ready record, concurrent ids, credential redaction, controlled shutdown/reaping |
| Network mode | No Chromium launch; separate transport and session counters |
| Hybrid mode | Browser context only after passing Go HTTP check; failed proxy retained as failure |
| Verified WSS via proxy | HTTP/1.1 upgrade and fixture message with both TLS profiles; no second origin TLS wrapper |
| MP4 access via proxy | Bounded range access with both profiles |
| Complete HLS decode via proxy | Two-second synthetic TS, all bytes transferred, 50 video frames and audio/duration evidence with both profiles |
| Slow script after HTTP 200 | Browser timeout retained; observed HTTP 200 and transport evidence preserved |
| HTTP 200 followed by late HTTP 403 | Session fails instead of reporting `navigation_succeeded` |

## External observations and remaining acceptance

A manual direct request to `https://tls.peet.ws/api/all` was attempted once per TLS
profile. Both connections reached the TLS stage and were reset. A separate direct
curl control also failed with `Recv failure: Connection reset by peer`. Therefore
this run did **not** establish remote JA3/JA4 equality; the precise cause of that
external reset is unknown. The local server-side ClientHello comparison passed.
The imported Chrome152 profile is not presented as an exact installed-Chrome match.

The free public proxy list was not exercised, so no new claim is made about the
user-reported 75% failure rate. The next acceptance run should use the same explicit
proxy file and seed for both profiles, preserve JSONL, compare per-stage failures,
and distinguish transport success from HTTP policy and browser readiness. Only
then can an actual before/after success-rate improvement be quantified.

No new 10/50/100 capacity benchmark was performed for this integrated version.
The earlier kickpop decoder measurements apply to their original workload and
measurement method. Current application CPU samples remain system-wide.

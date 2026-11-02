# Network engine and browser validation

Version 0.3 adds a Go network worker to Proxy Session Lab, using TLS/H2,
WebSocket, media-transfer and decoder components from kickpop. The Python CLI
owns candidate selection, admission, rate control, reporting and cleanup. One
persistent, bounded Go process handles network jobs over versioned NDJSON.

## Build and install

Keep the normal Python 3.11+ installation. Go **1.27+** builds the optional network
worker. Chromium is required for `browser` and `hybrid`; `network` does not launch
or require an installed browser. Full decoding additionally requires a local FFmpeg
executable (FFmpeg 7.1 was used in validation).

macOS/Linux:

```sh
./scripts/build_engine.sh
./run.sh https://your-app.example/health --engine network --direct --sessions 1
```

Windows PowerShell:

```powershell
.\scripts\build_engine.ps1
.\run.ps1 https://your-app.example/health --engine network --direct --sessions 1
```

The build scripts use `go` from PATH, or `PROXY_SESSION_LAB_GO`. No compiler,
FFmpeg or browser is installed automatically. To use another already-built worker,
pass `--engine-path /absolute/path/proxy-lab-engine`, set
`PROXY_SESSION_LAB_ENGINE`, or put `proxy-lab-engine` on PATH. Checkout-local
`engine/bin/proxy-lab-engine[.exe]` is detected automatically. Installing the Python
wheel alone does not bundle the Go binary; build it from a checkout or distribute
it separately. Python verifies the worker protocol before admitting network work.

## Modes

| Mode | Behavior |
| --- | --- |
| `browser` (default) | Existing isolated Chromium contexts and DOM readiness checks |
| `network` | Selected Go test, without Chromium |
| `hybrid` | Go HTTP validation first; a passing candidate then gets a Chromium context |

`--tls-profile stdlib` uses Go's native TLS; `--tls-profile chrome_152` uses the
pinned Chrome152 ClientHello profile and Chrome152 H2 SETTINGS/order/priority and
pseudo-header order when H2 is negotiated. TLS, ALPN and each measured phase are
reported. Headers are a synthetic, profile-consistent test set. This is a Chrome152
candidate, not a claim of exact equivalence to another installed Chrome version.
HTTP/3 and session resumption are not measured. When a JSON observer returns
`tls.ja4`, `tls.ja3_hash` or `http2.akamai_fingerprint`, the selected bounded
values are preserved under `observed`; ordinary target bodies remain only
in memory and are not logged as text.

The profile applies to **origin TLS through the proxy**, including an inner TLS
handshake for HTTPS proxies. TLS to an HTTPS proxy is a separate verified native
TLS handshake. An HTTP proxy URL remains `http://` when tunneling an HTTPS target.
WSS forces origin HTTP/1.1 ALPN for its upgrade, so its profile differs from the H2
HTTP measurement. Chromium always uses its own native TLS; the flag does not
rewrite Chromium's handshake. Use `network` or `hybrid` to select a Go profile.

## Proxy inputs and reproducibility

Create a file with one URL per line:

```text
http://proxy-host.example:8080
http://user:password@proxy-host.example:8081
https://secure-proxy.example:8443
socks5://user:password@socks-proxy.example:1080
```

Blank lines and `#` comments are allowed. Percent-encode credentials when needed.
The file is limited to 2 MiB; invalid lines fail with a line number without echoing
credentials. Explicit inputs can use hostnames and local addresses for controlled
lab tests. Proxifly retains its stricter global-IP/unauthenticated-HTTP filtering.
The public source remains the default when no explicit source is selected.

```sh
./run.sh https://your-app.example/health \
  --engine network --tls-profile chrome_152 \
  --proxy-file proxies.txt --sessions 20 --concurrency 4 --max-concurrency 4 \
  --seed 42 --expect-text healthy --log-file logs/network-chrome.jsonl
```

Repeat with `--tls-profile stdlib` and the same file/seed to compare the same
candidate order. Network conditions may still change between runs. Each candidate
is assigned at most once per run; failures consume the attempt budget. A seed fixes
ordering for the same candidate set, not endpoint health. `--proxy URL` can be
repeated for small explicit sets. A file avoids putting credentials on the command
line. URLs in reports and the run configuration omit credentials and query values;
credentials reach the Go worker on stdin and the selected proxy, never origin
request headers for a CONNECT tunnel.

`--direct` explicitly selects a direct reference instead of a proxy list. It can
run multiple independent sessions, uses no environment proxy in Go, and disables
Chromium's system proxy for a browser reference. Failed proxy jobs never fall back
to direct access.

Authenticated SOCKS5 is supported by the network worker. Chromium cannot use
SOCKS5 username/password authentication; such browser attempts are reported as an
unsupported browser configuration. Egress IP is only reported when a small JSON
response has an `ip` literal; the field is `reported_egress_ip`, not an independent
proof of anonymity or address change.

## Capability tests

`--check` is available with `--engine network`:

| Check | Scope |
| --- | --- |
| `http` (default) | TCP, optional proxy TLS, CONNECT/SOCKS5, verified origin TLS, HTTP response and bounded body |
| `ws` | Verified `wss://` TLS and HTTP/1.1 upgrade; no application subscription or publication |
| `media` | Existing HTTPS MP4/HLS bounded access probe (5s, 1s pause); validates range/prefix behavior, not full playback |
| `decode` | Full finite HTTPS TS byte-range HLS transfer into a paced FFmpeg software decoder; null audio/video output |

```sh
./run.sh wss://your-app.example/events --engine network --check ws \
  --proxy-file proxies.txt --sessions 2 --tls-profile chrome_152
./run.sh https://your-app.example/video.mp4 --engine network --check media \
  --proxy-file proxies.txt --sessions 2
./run.sh https://your-app.example/clip/playlist.m3u8 --engine network --check decode \
  --decoder /absolute/path/to/ffmpeg --navigation-timeout 90 --max-duration 120 \
  --proxy-file proxies.txt --sessions 1
```

Media checks require a **direct media URL**; this integration does not resolve a
Kick page or manipulate platform counters. Full decoding retains kickpop's finite
transfer limits: 60 seconds per manifest, 64 segments, 64 MiB total, 8 MiB per
segment, same-origin HTTPS media. Master/fMP4/live/encrypted/discontinuous full
decode is not supported. Video and audio are both required; frame/sample and
manifest-duration coverage must pass before a decode job is successful. No
presentation device or player-buffer stall measurement is included.

## Results and resource limits

A network HTTP check emits `network_check` with:

- `stages`: `tcp`, optional `proxy_tls`, `connect` or `socks5`, `tls`, `http`, `body`;
  each has duration, success and an error code. TCP includes proxy-host DNS where
  necessary; origin hostnames for CONNECT/SOCKS5 are resolved by the proxy.
- `transport_ok`: an HTTP response and bounded body were received over the selected
  verified transport. This can be true for HTTP 403 or a content-marker mismatch.
- `target_ok`: HTTP 2xx/3xx, no explicit challenge indication, and an optional
  `--expect-text` marker within the bounded response. Redirects are recorded as
  their immediate response; the Go HTTP check does not follow them.
- `failed_stage`/`error_code`: for example `connect/proxy_auth_required`,
  `tls/certificate_rejected`, `http/timeout`, or `target_http/target_http_error`.
- Negotiated TLS/HTTP metadata, bounded body byte count/hash and truncation flag;
  no raw body or raw exception is written. HTTP bodies are capped at 64 KiB + one
  sentinel byte, HTTP/1 headers at 32 KiB, and H2 headers by the profile's limit.

For WSS/media/decode the `details` field preserves the imported component's report;
its capability scope is explicit. Failure of those checks is not evidence that
basic HTTP proxy access failed. Network `success_percent` remains **session
outcome**; the report additionally supplies `transport_checks_success_percent`
for HTTP transport checks. A probe and target are separate checks in that metric.
Browser sessions preserve `transport_ok` when a main response was received even
if DOM readiness later timed out. A late HTTP error during the settle period now
fails the session. The `navigation` event keeps an observed HTTP status on timeout.

Target/probe HTTP 429/503 still stops the run. A rejected CONNECT is classified at
its proxy stage and is not relabeled as a target response. No application retry loop,
proxy rotation to continue past a target stop, or certificate-verification bypass
is added.

Concurrency, admission, rate and overall deadline still apply. Network/hybrid is
bounded to 100 workers; request timeout is at most 60s (full decode: 120s). The
overall `--max-duration` includes setup, rate waits and all check phases. Browser
and hybrid retain the five-second settle default; network has no settle delay.
Lightweight HTTP/WSS admission reserves 10% of RAM plus 32 MiB; browser/media/decode
retains 25% plus 128 MiB. `--ram-reserve-percent 0..90` explicitly overrides
the percentage; the per-mode admission margin remains. The run configuration
and dry-run record the effective reserve. Rate tokens cover both Go checks and browser navigations;
hybrid with a probe performs both network and browser validation.

### Hybrid scheduling and stage diagnostics

The single global rate gate prioritizes a browser navigation that is ready after
its Go check over network checks that have not started. FIFO order is retained
within each priority. This avoids putting all browser navigations behind a full
wave of prechecks while preserving the configured global start interval, bounded
worker count, overall deadline, and one-attempt-per-candidate budget.

`navigation_started`, `navigation`, `network_check_started`, and `network_check`
events carry `session_number` and rate-wait evidence. The first browser navigation
after a passing precheck additionally reports `hybrid_handoff_s`; its bounded
aggregate appears under `performance.hybrid_handoff`. Rate-gate waiters are
reported separately from the work queue, including browser/network counts.

HTTP results distinguish `http_response_received`, `body_read_ok`, and
`body_complete`. A successful bounded prefix read has `body_read_ok=true`; if the
64 KiB cap was reached, `body_complete=false` and `body_truncated=true`. HTTP 200
followed by a body timeout retains its HTTP evidence, but remains a failed target
check. Existing `transport_ok` still requires a successful bounded body read;
`target_ok` and session success are not relaxed.

The total `timeout` counter includes both Go and browser timeouts. Separate
`network_timeout`, `browser_timeout`, `timeouts_by_stage`, and `network_stages`
explain the breakdown. An expired/cancelled context is preserved when socket
closure races with I/O errors, rather than turning a deadline into a generic
connection error.

Browser lifecycle events (`context_opening`, `context_opened`, `context_closing`,
`context_closed`/`context_close_failed`) and main-document
`browser_request_failed` events correlate failures by session. A local-only
interface/address configuration digest is sampled during the run and again at
request failure. `network_state` / `network_state_change` record only a hash and
interface counts: no IPs, MACs, or interface names are written, and no external
IP-check request is added. This sensor does not cover every routing/DNS change,
nor does it prove the cause of Chromium's `ERR_NETWORK_CHANGED`. Transient changes
between samples may be missed. There is no automatic browser retry or error bypass.

For an engine comparison, use the same owned endpoint, explicit proxy file and
seed for each mode/profile, preserving each run in a separate JSONL file. Changing
the free-list sample or target URL cannot isolate an integration regression.

The existing CPU sample is system-wide, and helper RSS now includes the Go worker
and any FFmpeg children. Those values are not comparable to kickpop's per-cohort
CPU percentages without normalizing the measurement. Overlapping per-job Go CPU
inside decoder details must not be summed. Cancellation closes the Go worker,
its in-flight sockets, browser contexts and context-bound FFmpeg children.

## Tests

```sh
./scripts/build_engine.sh
.venv/bin/python -m unittest discover -s tests -v
cd engine
go test -race ./...
go vet ./...
```

Real Go integration tests skip with an explicit message if the worker is not built.
They use loopback proxies and synthetic origins; no free proxy list or third-party
target is tested. Decoder integration is opt-in with `KICKPOP_FFMPEG` and
`KICKPOP_DECODER_FIXTURE` pointing to a local FFmpeg and two-second synthetic TS.
Native Windows acceptance remains separate from a successful cross-build.

## Standalone worker

The Go binary can also run one check directly, without the Python scheduler:

```sh
engine/bin/proxy-lab-engine check --url https://your-app.example/health \
  --proxy http://proxy.example:8080 --tls-profile chrome_152 --timeout 10
```

`serve --workers N` is the Python bridge protocol: a ready/version record followed
by one result for each request id; stdin `{"control":"shutdown","id":"shutdown"}`
cancels active work and joins workers. One-shot failure returns a nonzero exit
status and a structured result. [Imported component provenance](../engine/PROVENANCE.md).

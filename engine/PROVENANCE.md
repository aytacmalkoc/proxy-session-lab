# Imported components

This engine incorporates code from the same author's
[aytacmalkoc/kickpop](https://github.com/aytacmalkoc/kickpop), based on its initial
published `main` commit `20c7652` (2 November 2026).

Imported packages: `internal/mediaprobe`, `internal/playbackaudit`,
`internal/wstransport`, their fixtures/tests, and the pinned Go dependency set.
Imports were relocated to this repository's engine module. HTTP/HTTP2/TLS profile
selection uses the same pinned fhttp/uTLS/tls-client profile implementations.

Integration adds explicit proxy transport, proxy-stage reports, a versioned NDJSON
worker, Python bridge, custom candidate inputs, and proxy propagation into the
media/decoder/WSS components. The existing Python bounded scheduler, resource
admission, rate gate, browser ownership and JSONL reporting remain the orchestration
layer; Go's worker pool is bounded to the same admission ceiling.

The source is maintained here rather than relying on an adjacent kickpop checkout,
module replacement, installed binary from another project, or an embedded Git
repository. Imported dependency versions remain pinned in `go.mod` and `go.sum`.

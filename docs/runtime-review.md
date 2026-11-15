# Cursor plugin runtime review

This unreleased candidate changes only the Cursor plugin. It does not modify
CPA, account data, production configuration, retries, or the schema-v1 C ABI.
All verification uses synthetic credentials, fixtures, or local sockets.

## Reproduced problems and changes

1. Stream failures previously closed the host stream with a plain string, losing
   status and retry metadata. The plugin now also sends the existing host
   `error_detail` schema, retaining the legacy string for older hosts. Errors
   carry upstream codes and Retry-After hints. Explicit HTTP error statuses take
   precedence over text heuristics; quota scope requires explicit structured
   evidence. Broad plan/weekly-usage text no longer becomes model-not-found or
   account-wide failure. Valid empty or metadata-only Connect end frames remain
   successful. Regression tests first failed on these cases before correction.
2. A completed tool batch could return before later fields in the same received
   Connect frame, losing available TurnEnded usage/cache counters or hiding a
   later protocol error. Completion now follows processing of the current frame.
   It does not read another frame or wait for TurnEnded on ordinary tool handoff;
   missing upstream usage still uses the explicitly marked estimate.
3. The optional `host-default` mode omits the forced HTTP/1.1 wire profile and
   delegates transport policy to CPA. The default remains `http1`. Configuration
   is validated before replacement and copied into each request scope, so an
   invalid reload leaves the active setting intact and existing requests retain
   their original mode. The only added Go dependency is pinned yaml.v3 for the
   host lifecycle YAML configuration.

## Configuration

```yaml
plugins:
  configs:
    spx-cursor:
      enabled: true
      http-transport: host-default
```

Use `http1` or remove `http-transport` to restore the compatibility default. CPA
still selects proxies, TLS, connections, logging and cancellation. Connection
reuse depends on CPA's credential pool/context/proxy configuration; omitting the
wire profile does not itself create a shared pool. RunSSE/BidiAppend and stateless
history reconstruction remain in use.

## Validation

Final integration checks passed on Windows AMD64 with Go 1.26.10 and MinGW:

```text
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build -buildmode=c-shared -o dist/spx-cursor.dll .
python test/abi-smoke.py dist/spx-cursor.dll
python -m unittest discover -s test -p 'test_*.py'
git diff --check
```

The Python packaging suite ran nine tests, with one platform-specific skip. The
real DLL passed C ABI registration, auth parsing, quota HTTP callbacks, status
mapping, callback-buffer ownership and shutdown checks. This validates the local
Windows library; Linux must be built and checked for its own target platform.

The final usage regressions were also replayed with only the decoder restored
from baseline `5f38f38`: 15 of 18 tool/usage ordering subtests failed, and the
late-protocol-error regression failed. The integrated decoder passes these same
tests. The error-mapping regression suite failed before its corrective patch on
valid metadata end frames, quota/permission classification and inferred quota
scope, then passed after correction.

Fixtures cover structured errors, valid end metadata, same-frame
usage ordering, zero/full-cache counters, stream/nonstream output, HTTP/2 and
HTTP/1.1 on local TLS sockets, concurrent RunSSE/BidiAppend, cancellation,
configuration reload and host-provided proxy/account separation.

## Evidence limits and follow-up

The production application-log sample contains failed execution attempts, not a
complete denominator of client requests. HTTP 200 access records do not prove
complete streams. Its remote Envoy-style errors do not identify Cursor edge
versus an intermediate proxy. It contains no response usage, cache counters,
TurnEnded, TTFT or finish-reason evidence that could establish the frequency of
estimated accounting or the cause of low reported cache reads.

Local HTTP/2 tests do not establish real Cursor endpoint support. These patches
do not promise lower latency, fewer remote 503s, or higher upstream cache hit
rates. Changing accounts/conversation IDs is only a candidate cache-affinity
factor. Ordinary tool steps may have no TurnEnded before caller tools run, so
waiting for more frames would risk blocking the caller. Recovering those counters
through dashboard events/deferred accounting needs a separate design.

A production improvement claim requires a controlled canary that captures final
usage source, cache counters, complete stream termination, TTFT, retry attempts,
connection reuse and remote response metadata without user messages or secrets.
CPA wire-profile connection pooling and retry policy remain separate CPA changes.

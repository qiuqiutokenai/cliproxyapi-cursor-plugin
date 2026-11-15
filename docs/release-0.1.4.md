# Cursor plugin 0.1.4

This is a custom build based on upstream 0.1.3 and the reviewed implementation
at commit `261c82f`. The runtime and registry now report `0.1.4`, so CPA can
distinguish this build from the original plugin. No public release or upstream
store update is implied by this version change.

## Changes

- Preserve structured stream errors, original HTTP statuses, error codes and
  Retry-After hints for CPA's existing retry and cooldown logic.
- Accept valid empty or metadata-only Connect end frames.
- Process all fields in the current received frame before returning a completed
  turn or tool batch. Already received token/cache counters are retained without
  waiting for another frame or delaying normal tool handoff.
- Add the opt-in `http-transport: host-default` setting. The default remains
  `http1`; this release does not change the selected account, conversation key,
  prompt history or upstream cache policy.

## Validation and limits

The reviewed implementation was exercised against CPA v8.0.66 with GPT-5.1 and
Gemini. In the final Gemini candidate probe, five independent conversations sent
three rounds with a shared send barrier. All 15 responses contained content,
usage, a stop reason, `[DONE]` and EOF. Eight of ten non-initial turns reported
positive cache reads. One Flash response took 27.3 seconds to produce content.
The baseline used different prompt seeds and ten non-initial turns, so this
small comparison does not establish a cache-rate or latency improvement.

The 0.1.4 version change requires freshly built libraries. A renamed 0.1.3
library is not a 0.1.4 artifact. Each delivered package must identify its source
commit, platform, library SHA-256 and validation results. The repository's
public release workflow requires all five native platform packages; a local
Linux AMD64 or Windows AMD64 package is only that platform's build.

`host-default` is experimental: it delegates protocol negotiation to CPA. It
does not force HTTP/2 or add a plugin-owned connection pool. An explicit CPA
proxy can still create a transport per operation. The private Cursor endpoint's
negotiated protocol and connection reuse were not measured in the probes.

## Custom deployment

CPA v8.0.66 exposes store installation, not a general local ZIP upload endpoint.
For a manually built library, back up the active library and plugin configuration
outside all plugin discovery directories, stop or replace one CPA instance, and
install the matching platform library in the configured persistent plugin
directory. A normal versioned Linux AMD64 target is:

```text
<plugins-dir>/linux/amd64/spx-cursor-v0.1.4.so
```

If `plugins.configs.spx-cursor.store.version` or `store.release-tag` pins the
previous version, it must not continue selecting 0.1.3. Follow the deployment
guide for the actual configuration rather than leaving both versions and
guessing which was loaded. Restart the instance and verify the registered
version is `0.1.4`, the loaded path is correct, and the installed library hash
matches the delivered artifact. Use `http-transport: http1` for the first rollout.

Keep the backup outside the discovery directories: CPA can remove unselected
libraries with the same plugin ID. To roll back, stop the affected instance,
restore the old library and its original configuration, then restart and verify
the old version and hash.

The registry URLs still identify the upstream project. Installing from that
source downloads its published assets, not this custom local build. To publish
a maintained fork through the store, update the repository/source metadata to
the actual fork and publish its matching platform packages before configuring
that source. Home synchronization and deployment init containers must also
agree with the selected custom artifact or they may restore another build.

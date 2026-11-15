# CLIProxyAPI Cursor Plugin

Connect Cursor to CLIProxyAPI with streaming, images, tool calls and quota reporting.
Uses CPA's global proxy and request logging. Native Go plugin; no Node.js runtime required.

[中文说明](README_CN.md)

## Store installation

After a Release is published, add this third-party source:

```yaml
plugins:
  enabled: true
  dir: /opt/cpa/plugins
  store-sources:
    - "https://raw.githubusercontent.com/spancerxing/cliproxyapi-cursor-plugin/main/registry.json"
```

Install **Cursor** in CPA's store. Release builds target Linux AMD64/ARM64,
macOS AMD64/ARM64 and Windows AMD64; CPA selects its runtime platform, including the Linux container architecture
for Docker. No build tools are needed at runtime. Mount a writable plugin directory for Docker.
Once listed in the official registry, the third-party source is optional.

## Build from source

Requires Go 1.23+, a C compiler and Make. Build on the same OS and CPU architecture as CPA:

```sh
git clone https://github.com/spancerxing/cliproxyapi-cursor-plugin.git
cd cliproxyapi-cursor-plugin
make build
```

Output in `dist/`: `spx-cursor.so` on Linux, `spx-cursor.dll` on Windows, or `spx-cursor.dylib` on macOS.
Windows builds require a matching MinGW toolchain.

## Install in CPA

CPA must support C ABI v1 plugins. Copy the library into the plugin directory and enable it in CPA's configuration:

```yaml
plugins:
  enabled: true
  dir: /opt/cpa/plugins
  configs:
    spx-cursor:
      enabled: true
      priority: 1
```

Set `dir` to the actual path, restart CPA and check its loading logs.
For Docker, mount the plugin directory and use its container path.
Manual installation requires the compiled library; CPA cannot load a source directory or a GitHub repository URL directly.

## Optional host transport

The default `http1` mode keeps the existing HTTP/1.1 wire profile. To test CPA's
normal HTTP transport without changing CPA, opt in explicitly:

```yaml
plugins:
  configs:
    spx-cursor:
      enabled: true
      http-transport: host-default
```

`host-default` omits the wire profile on host HTTP callbacks. CPA still owns
proxy selection, request logging, TLS, connections and cancellation. The host
may negotiate HTTP/2 or use HTTP/1.1; this does not switch the Cursor protocol to
the separate bidirectional `AgentService/Run` endpoint. RunSSE/BidiAppend and
stateless history reconstruction stay in use. No automatic retry is added when
a protocol or upstream error occurs.

Connection reuse depends on CPA's proxy and context transport. Its C ABI HTTP
callback currently passes no credential to the host client, so API-key pool
settings do not enable an account pool for this callback. An explicit global
proxy can still create a new transport per operation; a shared context/default
transport may reuse connections when no explicit proxy takes precedence.
This option does not create a
plugin-owned connection pool or guarantee reuse, lower latency or fewer 503s.
Local tests cover HTTP/1.1, HTTP/2, concurrent RunSSE/BidiAppend, cancellation and
host-provided account/proxy isolation; real Cursor endpoint support has not been
verified. Set `http-transport: http1` or remove the option to restore the default.
Each active request keeps the transport mode it started with during reconfigure.

## Sign in

Choose Cursor login in CPA. If the UI has no entry, use CPA's v8 management API:

```sh
curl 'http://127.0.0.1:8317/v8/management/oauth/auth-url?provider=cursor' \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
```

Open the returned `url` to authorize, then poll with the returned `state` until `status` is `ok`:

```sh
curl "http://127.0.0.1:8317/v8/management/oauth/status?state=$STATE" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
```

### API key login

After installing the plugin, open **Cursor** in CPA's plugin resources menu.
Enter the CPA management key, a Cursor API key and an optional account label,
then choose **验证并保存** (validate and save). The plugin validates the key with
Cursor before asking CPA to persist the account. Refresh the account and model
lists afterward. The page does not store keys in browser storage. Browser login
remains available through CPA's Cursor login entry.

The default resource URL is `/v0/resource/plugins/spx-cursor/login`.
Use the resource link in your management panel if CPA uses a different prefix.

Alternatively, import a Cursor API key credential into CPA:

```json
{
  "type": "cursor",
  "cursor_auth": {"type": "api", "key": "YOUR_CURSOR_API_KEY"}
}
```

## Use

Get model IDs from CPA's `/v1/models` and select a `cursor/…` model in your client.
Browser logins renew their access token through CPA's credential refresh about a day before it expires;
refresh preserves the account's enabled/disabled setting. If Cursor returns an unusable token or
rejects renewal, the plugin keeps the current token and stops renewal, including after a restart.
The current token can be used until it expires (unless Cursor revokes it earlier); sign in again
to resume renewal. Temporary network/server failures remain eligible for later refresh attempts.
Per-account proxy overrides are currently unsupported.

## Development

```sh
make test
```

See [VALIDATION.md](VALIDATION.md) for verification scope and limitations.
Real Cursor accounts and full CPA integration have not yet been tested.
Based on the MIT-licensed [magpie Cursor plugin](https://github.com/magpie-community/plugins/tree/main/packages/cursor); see [NOTICE.md](NOTICE.md) for attribution.

See [docs/publishing.md](docs/publishing.md) for releases and official store submission.

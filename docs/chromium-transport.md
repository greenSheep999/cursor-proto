# Chromium transport for Cursor Claude access

## Status

Cursor's current edge returns different entitlements for the same account and
the same protobuf request depending on the client transport implementation.
The native Go transport receives 24 models and no Claude models; a real
Chromium transport receives 35 models including 11 Claude models.

This is not an HTTP/2-only behaviour. Chromium continues to receive the full
catalog when QUIC and HTTP/2 are disabled and the request negotiates HTTP/1.1.
It is also not caused by cookies, browser storage, User-Agent, Fetch Metadata,
`x-cursor-team-id`, or the `AvailableModelsRequest` picker flags.

The production workaround is therefore an optional, loopback-only Chromium
sidecar. The CPA plugin and `cursor-proxy` keep all Cursor protocol knowledge;
the sidecar only supplies the outbound browser network stack.

## Verified evidence

All rows below used the same local account token and protobuf body.

| Transport | Negotiated protocol | Models | Claude |
|---|---:|---:|---:|
| Go `net/http` | HTTP/2 | 24 | 0 |
| Chromium default | HTTP/2 | 35 | 11 |
| Chromium with Go User-Agent | HTTP/2 | 35 | 11 |
| Chromium without `sec-*`, Origin, Referer, Priority | HTTP/2 | 35 | 11 |
| Go with Chromium's browser headers copied | HTTP/2 | 24 | 0 |
| Chromium with QUIC disabled | HTTP/2 | 35 | 11 |
| Chromium with QUIC and HTTP/2 disabled | HTTP/1.1 | 35 | 11 |
| Chromium through a TLS-terminating MITM | HTTP/2 | 24 | 0 |
| Chromium without `x-cursor-team-id` | HTTP/2 | 35 | 11 |

The strongest isolation is the MITM result: the application request remains a
Chromium fetch, but replacing Chromium's server-facing TLS/connection identity
with the MITM implementation changes the response from 35/11 to 24/0.

The enforcing layer may belong to Cursor or an upstream edge/security vendor.
The experiments establish transport-level classification; they do not identify
the vendor implementing it.

## Architecture

```text
New API / OpenAI / Anthropic client
                 |
          CPA host or cursor-proxy
                 |
       Cursor protobuf executor
          |              |
          | native Go    | CURSOR_CHROMIUM_SIDECAR_URL
          | fallback     v
          |       loopback Chromium sidecar
          |              |
          +--------------+--> api2.cursor.sh
                              AvailableModels
                              RunSSE
                              BidiAppend
```

The seam is URL routing, not a second Cursor protocol implementation:

- Go constructs authorization, `x-cursor-*` headers, protobuf messages,
  Connect envelopes, request IDs, conversation state, and tool results.
- Chromium performs `fetch()` and streams response bytes back unchanged.
- Go continues to decode Connect frames and translate events into OpenAI,
  Anthropic, Gemini, and Responses-compatible output.

This keeps the sidecar a deep transport module with a deliberately small
interface:

```text
POST /api2/<cursor RPC path>
POST /api3/<cursor RPC path>
GET  /healthz
```

Only `api2.cursor.sh` and `api3.cursor.sh` are valid upstream targets. The
sidecar must listen on loopback and must never be exposed as a general proxy.

## Configuration

CPA plugin:

```bash
export CURSOR_CHROMIUM_SIDECAR_URL=http://127.0.0.1:18901
```

Standalone `cursor-proxy`:

```bash
cursor-proxy \
  -chromium-sidecar-url http://127.0.0.1:18901 \
  -addr 127.0.0.1:8317
```

Equivalent environment variable:

```bash
export CURSOR_CHROMIUM_SIDECAR_URL=http://127.0.0.1:18901
```

When configured, model discovery and all chat RPCs use the sidecar. When it is
unset, behaviour remains unchanged and the native Go transport is used.

The sidecar itself is configured with:

| Variable | Default | Meaning |
|---|---:|---|
| `CURSOR_CHROMIUM_LISTEN` | `127.0.0.1:18901` | Loopback listen address |
| `CURSOR_CHROMIUM_MAX_CONCURRENCY` | `8` | Maximum simultaneous browser fetches |
| `CURSOR_CHROMIUM_REQUEST_LIMIT` | `16777216` | Maximum request body bytes |
| `CURSOR_CHROMIUM_RESPONSE_START_TIMEOUT_MS` | `20000` | Abort if Cursor returns no response body chunk within this many milliseconds |
| `CURSOR_CHROMIUM_EXECUTABLE_PATH` | Playwright default | Explicit Chrome/Chromium executable |
| `CURSOR_CHROMIUM_SIDECAR_TOKEN` | empty | Optional shared token required from callers |

If a token is configured, the plugin/proxy reads the same variable and sends
it only to the loopback sidecar. The header is removed before the Cursor
request is issued.

## Routing policy

The first production version routes the whole Cursor executor through the
sidecar when enabled, rather than only requests whose model name contains
`claude`.

Reasons:

1. `AvailableModels` must see the same entitlement as chat execution.
2. Model aliases and routed model updates can select a provider after the
   initial request; model-name string matching is not authoritative.
3. RunSSE and BidiAppend are a paired protocol and must use one consistent
   transport identity.
4. A single routing rule is easier to observe, test, and roll back.

Native Go remains the explicit fallback when the sidecar URL is absent. The
first version does not silently retry a failed Chromium request through Go,
because doing so can turn a real Claude error into a misleading empty response.

## Per-account catalog shapes

Cursor does not return one universal model-catalog shape. The shape can differ
between accounts on the same machine and can change when the active account is
switched:

- A parameterized catalog returns one primary row such as `claude-opus-5` and
  puts effort, thinking, context, and fast-mode choices in `Variants`.
- An exploded catalog returns each choice as its own `Name`, such as
  `claude-opus-5-high` or `claude-opus-5-thinking-max-fast`.

Compatibility is therefore resolved per auth from its live catalog, never from
a process-wide assumption about which shape Cursor is currently using:

1. `AvailableModelIDs` folds either shape into compact primary names for normal
   user-facing model lists.
2. `RoutableModelIDs` registers the primary names plus every live variant slug
   with CPA. This matters because CPA selects a provider before invoking the
   plugin; an omitted variant would otherwise fail as `unknown provider`.
3. Chat execution resolves the requested name back against that same account's
   live catalog and sends Cursor's exact `parameter_values`, preserving effort,
   thinking, context, and fast-mode semantics.
4. Duplicate names are removed, while short marketing aliases that are not
   actual children of a primary model are not claimed automatically.

This covers mixed pools where one account returns primary rows and another
returns flattened variants without maintaining account-specific mapping files.

## Process and resource model

- One long-lived Chromium browser per sidecar process.
- One isolated browser context shared by stateless pages.
- One page per active HTTP request in the initial implementation.
- A bounded semaphore limits concurrent pages.
- Closing the downstream connection closes the page and cancels its fetch.
- Response chunks respect Node HTTP backpressure before the next browser chunk
  is requested.
- Browser crashes make `/healthz` fail and the process exits so systemd,
  Docker, or another supervisor can restart it.

`/healthz` also reports credential-safe route counters:

- `direct_requests` and `proxied_requests` count resolved request routes;
- `last_route` is `direct`, `socks-bridge`, `socks-proxy`, or `http-proxy`;
- `proxy_bridge_count` reports the number of reusable authenticated SOCKS
  bridges without revealing their endpoints or credentials.

These fields distinguish a missing per-account proxy handoff from an upstream
empty response after Chromium has already selected the intended proxy route.

The response-start timeout is deliberately shorter than Cloudflare's
120-second proxy read timeout. If one Cursor account accepts a request but
never starts a response body, the sidecar aborts that browser fetch and returns a
retryable upstream error while CPA still has time to select another auth
record. The timer stops when the first body chunk arrives, so it does not cap a
healthy long-running stream.

Cookies and persistent profiles are intentionally disabled. Live experiments
show the entitlement does not depend on browser storage, and stateless contexts
avoid cross-account leakage when several CPA auth records share one sidecar.

## New API compatibility

No New API-specific protocol fork is needed. New API speaks an OpenAI-compatible
surface to CPA or `cursor-proxy`; the existing translators already produce:

- `/v1/chat/completions`
- `/v1/responses`
- `/v1/completions`
- `/v1/messages`
- Gemini-compatible routes

Once the executor's three Cursor RPCs use Chromium, those downstream surfaces
receive the same non-empty Claude events as the local prototype.

### Internal New API to CPA routing

When New API and CPA run on the same Docker host, CPA-backed New API channels
must use the shared Docker network rather than CPA's public Cloudflare domain:

```text
New API client
    |
    v
api.100b.best -> New API -> http://cli-proxy-api:8317 -> CPA Cursor plugin
                                      |
                                      v
                              Chromium sidecar -> Cursor
```

Using `https://cpa.example.com` as a New API channel base URL sends same-host
service traffic out through Cloudflare and back to CPA. Cloudflare can then
intermittently return an HTML challenge or edge error instead of an API JSON
response. A typical symptom is a retrying `non_stream` client receiving HTTP
403 with `Just a moment...` and `challenges.cloudflare.com` in the body. That
response is generated before the CPA plugin or Cursor executor handles the
request; retrying the model request does not repair the route.

The production rules are:

1. Attach New API and CPA to one private Docker network.
2. Set every CPA-backed New API channel base URL to
   `http://cli-proxy-api:8317` (or the deployment's equivalent service name).
3. Keep CPA's public domain for real external CPA clients only.
4. If New API has an SSRF egress lockdown, add a narrow allow rule for TCP
   traffic from New API to CPA port 8317 before the private-network drop rule.
   Do not disable the rest of the SSRF policy.
5. Resolve and verify the current container addresses before installing an
   address-based firewall rule. Reapply or regenerate the rule whenever a
   container is recreated with a different address.

Before changing channel rows, save their current values in a backup table or
an equivalent recoverable snapshot. Verify the route from inside the New API
container before switching production traffic:

```bash
wget -S -O /dev/null \
  --header="Authorization: Bearer $CPA_API_KEY" \
  http://cli-proxy-api:8317/v1/models
```

After switching, test both streaming modes for `/v1/chat/completions` and
`/v1/responses`. Acceptance requires HTTP 200, no HTML challenge body, the
expected completion marker, and a terminal finish/completed event. Chat and
non-streaming Responses responses should include usage. Streaming Responses
usage depends on the CPA host conversion layer and is not a Chromium transport
health signal.

## Operational requirements

The sidecar requires Node.js and Playwright Chromium. Install dependencies in
the sidecar directory and install the matching browser:

```bash
npm ci
npx playwright install chromium
```

Linux hosts may need:

```bash
npx playwright install --with-deps chromium
```

For a containerized CPA deployment, build the included `Dockerfile` and run
the sidecar in the CPA container's network namespace. The sidecar can then
remain bound to `127.0.0.1:18901`, and the plugin uses the same loopback URL
as a non-container deployment. Configure the same shared token in both
containers and do not publish the sidecar port.

Do not set `HTTPS_PROXY` for the sidecar to a TLS-terminating MITM. That changes
the server-facing transport identity and reproduces the 24-model response.

## Verification

Transport matrix:

```bash
go run ./cmd/test-model-stack
```

Real CPA plugin chat through the production sidecar:

```bash
cd chromium-sidecar
npm start

# In another shell:
CURSOR_CHROMIUM_SIDECAR_URL=http://127.0.0.1:18901 \
  go run ./cmd/plugin-e2e -skip-build \
    -model claude-opus-5 \
    -format openai \
    -msg 'Reply with exactly: CHROMIUM_OK'
```

Acceptance for the production integration:

1. CPA `model.for_auth` advertises the full browser-visible catalog.
2. OpenAI streaming and non-streaming requests return non-empty Claude output.
3. Anthropic streaming and non-streaming requests return non-empty output.
4. `/v1/responses` completes with valid Responses events.
5. Tool calls still pair RunSSE and BidiAppend correctly.
6. Sidecar cancellation leaves no orphan page.
7. Native Go behaviour is unchanged when the sidecar is not configured.

## Non-goals

- Reimplementing Chromium TLS or HTTP internals in Go.
- Claiming a specific edge/security vendor without evidence.
- Persisting browser profiles or account cookies.
- Exposing the sidecar outside localhost.
- Treating `team_id` or model picker flags as Claude entitlement fixes.

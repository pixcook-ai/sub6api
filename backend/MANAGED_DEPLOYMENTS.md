# Managed deployments

An administrator registers two immutable OpenAI account bindings in one OpenAI
API-key group: an API-key account for an independently running BPS worker and an
externally managed Codex OAuth account. Both accounts retain platform `openai`.
`selected_provider` is an explicit manual choice; it never enables, disables,
refreshes or replaces an account.

## Administrator API

`GET /api/v1/admin/managed-deployments/:id` returns the deployment.
`PUT` at the same path creates or switches it. Both use existing administrator
authentication, not a runtime controller key. Successful responses use the normal
`{code: 0, message: "success", data: ...}` envelope.

The complete PUT object contains `contract: "tosub2-managed-deployment/v1"`,
`deployment_id: "deployment_<uuid>"`, positive `group_id`,
`expected_route_revision: 0`, `route_revision: 1`,
`selected_provider: "bps_excel" | "codex"`, and both `bps` and `codex` bindings.
Each binding has `provider`, actual `account_id`, `backend_id`,
`binding_ref: "bind_<uuid>"`, `profile_ref: "oauth_<uuid>"`,
`identity_epoch: "<uuid>"`, `workspace_id` and `subject`.

To switch, send the complete same immutable configuration with
`expected_route_revision = n`, `route_revision = n+1`, and the requested provider.
An exact retry is idempotent; a conflicting revision or immutable identity returns
409. Each group belongs to one deployment. The BPS account's
`extra.managed_deployment_binding` contains binding fields **except account_id**;
that ID comes from the account row. Codex identity comes from `managed_oauth`.
Both account membership and immutable metadata are rechecked by the SQL write.

Configuration does not imply availability. Before presenting a switch as ready,
read the selected account's scheduling/status, verify managed Codex authorization
or BPS worker authorization, and obtain the user's explicit scheduling action
when needed. A paused selected account makes inference unavailable; it never
causes fallback. The API key and API URL remain unchanged.

## Sessions and resources

An inference request supplies official `session-id` (or `session_id` / `X-Session-ID`); absent these headers,
`prompt_cache_key` is the explicit session ID. There is no first-message/content
heuristic. A session is hashed and scoped to deployment, user, API key and group.
The first request atomically snapshots the selected complete binding while
serializing against manual selection changes. Subsequent requests keep that
binding permanently: no Redis TTL or process restart can move the session.

A request with no explicit session can continue using trusted resource ownership.
Every `previous_response_id`, file/container/vector-store reference, encrypted
reasoning and tool-call reference must have durable ownership under the same
full binding. An explicit fresh session snapshots the current choice; referencing
an old resource from another provider or epoch returns 409 and requires rebuilding
context. Missing owners are never guessed. Metadata, JSON schemas and reflected
function arguments/results are not sources of ownership.

Successful HTTP JSON/SSE resource IDs are persisted before delivery. GET/list
responses validate existing owners instead of claiming upstream-wide listings.
A resource endpoint must filter shared account lists and authorize parent objects
before deliberately registering any child resources. Binary downloads require
an already authorized input resource. WebSocket transports use the same payload
validation/recording helpers before upstream/downstream frame dispatch.

The scheduler reads the pinned account directly, revalidates full identity and
group membership, and still applies model, transport, scheduling, privacy, quota,
health, profit and concurrency gates. No excluded or unavailable pinned account
may be replaced. Managed requests do not permit inference replay or context
stripping; a 401 is reported for recovery of the original grant, never as a reason
to switch provider. Explicit tenant/key/group deletion may cascade its ledger;
there is no background expiry or rebinding.

## Validation

Focused Go tests use `-tags unit -run '^TestManagedDeployment'` in service,
handler, handler/admin and repository packages. The standalone
`scripts/test-managed-deployment-postgres.py` executes current repository SQL
and migration on a disposable PostgreSQL 17 container with no published ports or
network. It covers CAS, immutable identity, membership, multi-tenant ownership,
24 concurrent first requests, and concurrent selection/session creation. These
checks use synthetic fixtures and do not prove real OAuth or model inference.

## Unsupported endpoints in managed groups

Managed groups return HTTP 501 with fixed code
`managed_deployment_endpoint_unsupported` before task creation or upstream
dispatch for async images and image task status, Videos/Seedance job APIs,
Live/realtime calls, Alpha Search, and other routes outside the enabled
Responses/Chat/Messages/Files plus synchronous images/embeddings surfaces.
Discovery routes remain available. Ordinary groups retain their existing APIs.
The HTTP middleware tests include a real loopback upstream and ordinary-group
positive controls: the unsupported managed requests must create no job and send
no upstream request.

`ScopeManagedDeploymentAffinity(ctx, value)` returns a deterministic UUID scoped
to the deployment, group, user, API key, complete binding and durable session key.
Every actual HTTP/WebSocket serializer must apply it to upstream session headers,
prompt cache keys and client metadata. The resource ledger alone cannot isolate
a shared upstream account's wire state. Missing or failed scoped persistence
must not fall through to a regular account pool.

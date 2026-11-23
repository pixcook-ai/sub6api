# Official Claude Code local capture

Run the official darwin-arm64 package from the npm registry against a controlled
loopback Messages/SSE receiver. The script never invokes the installed claude
launcher, never uses an existing account, and never forwards a request upstream.

From the repository root:

    rtk proxy python3 scripts/claude-oauth-compare/capture_official.py \
      --version 2.1.330 \
      --artifact-dir .scratch/claude-oauth-compatibility-20261123/official-runtime \
      --fixtures-dir backend/internal/service/testdata/claude-official

Run relative paths from the repository root; the verified executable is
converted to an absolute path before the CLI changes into its isolated cwd.
To recheck this invocation without overwriting the committed fixtures:

    rtk proxy python3 scripts/claude-oauth-compare/capture_official.py \
      --version 2.1.330 \
      --artifact-dir .scratch/claude-oauth-compatibility-20261123/official-runtime \
      --fixtures-dir .scratch/claude-oauth-compatibility-20261123/official-runtime-recheck

Requirements: macOS arm64 and Python 3.11+ (tarfile data filter support).
The selected package is downloaded directly from registry.npmjs.org. Its SHA-512
must exactly match registry dist.integrity, and the executable must match the
verified archive member. No global installation or dependency installation occurs.
The tarball is about 164 MB; extracted native executable about 364 MB.

The two runs differ only by CLAUDE_CODE_ATTRIBUTION_HEADER being unset or 0.
Both use a temporary HOME, CLAUDE_CONFIG_DIR, work directory, synthetic OAuth
token, and CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1. HTTP/HTTPS/ALL proxy
variables point to the local receiver, which rejects external forwarding.
The receiver returns legal Messages/SSE with CAPTURE_OK. Each process must exit
successfully and produce at least one local Messages request. Server threads,
ports, and temporary directories close before the command exits.

Fixtures contain requests actually received by the server. Authorization values
are redacted. Metadata identity fields use fixed synthetic values; ephemeral
paths use a controlled-runtime placeholder. Original Content-Length is retained
as an observation and may differ from the sanitized fixture representation.
Do not compare Host, transport length, generated request IDs, or intentional
account identity rewriting as protocol failures.

manifest.json records package provenance, verified artifact hashes, fixture
hashes, environment boundaries, and completion. An npm registry signature
verification is not performed separately from HTTPS plus dist.integrity.

The test verifies client-to-local-server request construction and stream parsing.
It does not verify authenticated Anthropic inference, account enforcement,
upstream acceptance of rewritten requests, or all feature-flag branches.
Missing wiggly or other opaque fields in this isolated run is not evidence of
global absence or a claim that anti-abuse enforcement has been solved.

## Gateway comparison and focused regression

From the repository's backend directory, run the same focused package/test
selection without depending on a machine-local test-lock script:

    rtk proxy go test -json -p=1 -tags=unit \
      ./internal/service ./internal/handler/admin ./internal/server/routes \
      -run 'ClaudeOAuthCompatibility|ClaudeOfficialRuntimeLocalHTTP|AnthropicOAuth|ClaudeCodeFingerprint|BillingHeader|CountTokens|MessageOutputConfig' \
      -count=1 -timeout=120s

The official-runtime comparison has 12 cases and sends the captured requests
through GatewayService.Forward/ForwardCountTokens to a real local HTTP receiver.
The count_tokens cases adapt a captured Messages request to that endpoint;
they are not official CLI count_tokens captures. For native/body-recognized
cases, expected messages account only for removal of messages[1].output_config
when its required beta is absent; all other message fields remain strict,
and top-level output_config is retained.
Existing billing, mimic and endpoint-specific transformations still apply.
The runtime report records the observed matrix and the separate current-root
GREEN run; neither establishes authenticated upstream or TLS equivalence.

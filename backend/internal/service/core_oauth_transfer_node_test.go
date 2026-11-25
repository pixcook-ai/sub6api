package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

// This optional sibling-repository test exercises the production sender's
// crypto rather than reproducing its envelope construction in Go. It never
// contacts an OAuth provider or reads a real credential store.
func TestCoreOAuthTransferProductionNodeInterop(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for production sender interoperability")
	}
	module := coreOAuthNodeSenderModule(t)
	for _, provider := range []string{service.CoreOAuthProviderExcel, service.CoreOAuthProviderCodex} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
			protocol := coreOAuthProtocolFixture()
			if provider == service.CoreOAuthProviderCodex {
				protocol.grant.ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
			}
			protocols := map[string]service.CoreOAuthProtocol{provider: protocol}
			core := coreOAuthTestService(t, store, protocols)
			profile, err := core.CreateProfile(ctx, service.CoreOAuthCreateInput{Provider: provider, Name: "Node transfer integration"})
			require.NoError(t, err)
			in := service.CoreOAuthHandoffInvitationInput{
				OperationID: "11111111-1111-4111-8111-111111111111", ProfileRef: profile.ProfileRef,
				SourceProfileRef: "oauth_33333333-3333-4333-8333-333333333333", Provider: provider,
				Subject: protocol.grant.Subject, WorkspaceID: protocol.grant.WorkspaceID,
				IdentityEpoch: "22222222-2222-4222-8222-222222222222", TokenRevision: 7,
				ExpiresAt: time.Now().UTC().Add(10 * time.Minute), ActorID: 42,
			}
			invitation, err := core.CreateHandoffInvitation(ctx, profile.ProfileRef, in)
			require.NoError(t, err)
			identity := map[string]any{
				"contract": "tosub2-managed-oauth/v1", "backend_id": "node-interop-worker",
				"binding_ref": "bind_44444444-4444-4444-8444-444444444444",
				"profile_ref": in.SourceProfileRef, "provider": provider, "subject": in.Subject,
				"workspace_id": in.WorkspaceID, "identity_epoch": in.IdentityEpoch,
			}
			var proof map[string]any
			if provider == service.CoreOAuthProviderExcel {
				identity["token_revision"], identity["operation_id"] = in.TokenRevision, in.OperationID
				proof = map[string]any{"kind": "bps_worker", "account_id": 17, "receipt": identity}
			} else {
				proof = map[string]any{"kind": "codex_controller", "account_id": 17, "receipt": map[string]any{
					"account_id": 17, "operation_id": in.OperationID, "token_revision": in.TokenRevision,
					"identity": identity, "fenced": true,
				}}
			}
			g := protocol.grant
			payload := map[string]any{
				"contract": service.CoreOAuthHandoffTransferContract, "operation_id": in.OperationID,
				"profile_ref": in.ProfileRef, "source_profile_ref": in.SourceProfileRef, "provider": provider,
				"subject": in.Subject, "workspace_id": in.WorkspaceID, "identity_epoch": in.IdentityEpoch,
				"token_revision": in.TokenRevision, "source_quiesced": true, "publication_fences": []any{proof},
				"grant": map[string]any{
					"access_token": g.AccessToken, "refresh_token": g.RefreshToken, "id_token": g.IDToken,
					"client_id": g.ClientID, "subject": g.Subject, "workspace_id": g.WorkspaceID,
					"email": g.Email, "expires_at": g.ExpiresAt.Format(time.RFC3339Nano),
				},
			}
			envelope := coreOAuthSealWithProductionNode(t, node, module, invitation, payload)
			// The sender process has exited before any receiver activation or refresh.
			core = coreOAuthTestService(t, store, protocols)
			_, err = core.AcceptProtectedHandoff(ctx, in.ProfileRef, in.ActorID, envelope, nil)
			require.ErrorIs(t, err, service.ErrCoreOAuthUnavailable)
			_, err = core.CredentialSnapshot(ctx, in.ProfileRef)
			require.ErrorIs(t, err, service.ErrCoreOAuthHandoffPending)
			verifications := 0
			proofJSON, err := json.Marshal(proof)
			require.NoError(t, err)
			verifier := func(_ context.Context, verified service.CoreOAuthHandoffVerification) error {
				verifications++
				require.Equal(t, *invitation, verified.Invitation)
				require.True(t, verified.SourceQuiesced)
				require.Len(t, verified.PublicationFences, 1)
				require.JSONEq(t, string(proofJSON), string(verified.PublicationFences[0]))
				return nil
			}
			receipt, err := core.AcceptProtectedHandoff(ctx, in.ProfileRef, in.ActorID, envelope, verifier)
			require.NoError(t, err)
			require.Equal(t, "accepted", receipt.Status)
			require.Equal(t, 1, verifications)
			require.Zero(t, protocol.refreshes)
			snapshot, err := core.CredentialSnapshot(ctx, in.ProfileRef)
			require.NoError(t, err)
			require.Equal(t, g.AccessToken, snapshot.AccessToken)
			require.Equal(t, in.TokenRevision, snapshot.Profile.TokenRevision)
			require.Equal(t, provider, snapshot.Profile.Provider)
			require.True(t, snapshot.Profile.Authorized)
			// A fresh receiver uses its encrypted grant after the sender disappeared.
			core = coreOAuthTestService(t, store, protocols)
			refreshed, err := core.Refresh(ctx, in.ProfileRef, in.TokenRevision)
			require.NoError(t, err)
			require.Equal(t, in.TokenRevision+1, refreshed.TokenRevision)
			require.Equal(t, 1, protocol.refreshes)
			core = coreOAuthTestService(t, store, protocols)
			snapshot, err = core.CredentialSnapshot(ctx, in.ProfileRef)
			require.NoError(t, err)
			require.Equal(t, "synthetic-access-rotated", snapshot.AccessToken)
			retry, err := core.AcceptProtectedHandoff(ctx, in.ProfileRef, in.ActorID, envelope, verifier)
			require.NoError(t, err)
			require.Equal(t, receipt, retry)
			require.Equal(t, 1, verifications, "accepted replay must not invoke a source authority")
			require.Equal(t, 1, protocol.refreshes, "accepted replay must not consume a refresh token")
			persisted, err := core.GetHandoff(ctx, in.ProfileRef, in.OperationID, in.ActorID)
			require.NoError(t, err)
			require.Equal(t, receipt, persisted)
			for _, safe := range []any{receipt, retry, persisted, snapshot.Profile} {
				raw, err := json.Marshal(safe)
				require.NoError(t, err)
				for _, secret := range []string{g.AccessToken, g.RefreshToken, g.IDToken, "synthetic-access-rotated", "synthetic-refresh-rotated", "access_token", "refresh_token", "id_token", "private_key"} {
					if bytes.Contains(raw, []byte(secret)) {
						t.Fatal("safe receiver response exposed credential material")
					}
				}
			}
		})
	}
}

func coreOAuthNodeSenderModule(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		candidate := filepath.Join(dir, "toSub2", "src", "oauth-handoff-protection.mjs")
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		} else if err != nil && !os.IsNotExist(err) {
			t.Fatal("production Node sender module cannot be inspected")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("the sibling toSub2 production sender is required for interoperability")
	return ""
}

func coreOAuthSealWithProductionNode(t *testing.T, node, module string, invitation *service.CoreOAuthHandoffInvitation, payload map[string]any) service.CoreOAuthHandoffEnvelope {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"invitation": invitation, "payload": payload})
	require.NoError(t, err)
	defer clear(raw)
	// Arguments contain only code and the module path. Synthetic credential
	// plaintext travels through stdin and is never printed on a failed command.
	const script = `import { pathToFileURL } from "node:url";
try {
  const { sealOAuthHandoff } = await import(pathToFileURL(process.argv[1]).href);
  const chunks = []; let size = 0;
  for await (const chunk of process.stdin) {
    size += chunk.length; if (size > 512 * 1024) throw new Error(); chunks.push(chunk);
  }
  const bytes = Buffer.concat(chunks);
  const { invitation, payload } = JSON.parse(bytes.toString());
  bytes.fill(0);
  process.stdout.write(JSON.stringify(sealOAuthHandoff(invitation, payload)));
} catch { process.stderr.write("production sender sealing failed"); process.exitCode = 1; }`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", script, module)
	cmd.Stdin = bytes.NewReader(raw)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	sealed, err := cmd.Output()
	if err != nil {
		t.Fatal("production Node sender sealing failed; plaintext and subprocess output withheld")
	}
	require.True(t, cmd.ProcessState.Exited())
	require.True(t, cmd.ProcessState.Success())
	if len(sealed) > 256<<10 {
		t.Fatal("production Node sender returned an oversized envelope")
	}
	var envelope service.CoreOAuthHandoffEnvelope
	if json.Unmarshal(sealed, &envelope) != nil {
		t.Fatal("production Node sender returned an invalid envelope")
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "synthetic-access-original", "synthetic-refresh-original", "synthetic-id-token"} {
		if bytes.Contains(sealed, []byte(key)) {
			t.Fatal("production Node sender exposed plaintext credential material")
		}
	}
	return envelope
}

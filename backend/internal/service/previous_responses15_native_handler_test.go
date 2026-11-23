//go:build recovery15native

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func TestStoredResponseNativeParent15UsesFreshAccountAndDetachedSnapshot(t *testing.T) {
	svc, ctx, record, accounts, _ := native15ParentFixture(t)
	parent, err := svc.ResolveStoredResponseNativeParent15(ctx, StoredResponseOwner{1, 2, 3}, record.PublicID, "public", "public")
	if err != nil || parent == nil || parent.Account == nil || parent.Account.ID != 4 || parent.Binding.ResolvedModel != "resolved" || len(parent.Snapshot) == 0 {
		t.Fatalf("resolve: %+v %v", parent, err)
	}
	if accounts.reads < 2 {
		t.Fatal("owning account did not pass actual fresh authorization")
	}
	var material struct{ Snapshot []byte }
	json.Unmarshal(record.Recovery, &material)
	if !bytes.Equal(parent.Snapshot, material.Snapshot) {
		t.Fatal("serialized native snapshot changed")
	}
	parent.Snapshot[0] = 'x'
	parent.Account.Credentials["api_key"] = "caller local change"
	if accounts.current.GetCredential("api_key") == "caller local change" {
		t.Fatal("returned account shares mutable credentials with repository")
	}
	again, err := svc.ResolveStoredResponseNativeParent15(ctx, StoredResponseOwner{1, 2, 3}, record.PublicID, "public", "public")
	if err != nil || !bytes.Equal(again.Snapshot, material.Snapshot) {
		t.Fatal("one caller mutated durable recovery or current account")
	}
}

func TestStoredResponseNativeParent15RejectsCurrentAuthorityAndCredentialChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*OpenAIGatewayService, *StoredResponse, *native15ParentAccounts, *storedNativeAccountAuthorizationGroups)
		want   error
	}{
		{"current credential generation", func(_ *OpenAIGatewayService, _ *StoredResponse, a *native15ParentAccounts, _ *storedNativeAccountAuthorizationGroups) {
			a.current.Credentials["bps_responses_credential_generation"] = "generation-new"
		}, ErrBPSRequestAuthorizationRevoked},
		{"current backend", func(_ *OpenAIGatewayService, _ *StoredResponse, a *native15ParentAccounts, _ *storedNativeAccountAuthorizationGroups) {
			a.current.Credentials["bps_responses_backend_id"] = "backend-new"
		}, ErrBPSRequestAuthorizationRevoked},
		{"current model mapping", func(_ *OpenAIGatewayService, _ *StoredResponse, a *native15ParentAccounts, _ *storedNativeAccountAuthorizationGroups) {
			a.current.Credentials["model_mapping"] = map[string]any{"public": "different"}
		}, ErrBPSRequestAuthorizationRevoked},
		{"fresh group revoked", func(_ *OpenAIGatewayService, _ *StoredResponse, _ *native15ParentAccounts, g *storedNativeAccountAuthorizationGroups) {
			g.group.Status = StatusDisabled
		}, ErrBPSRequestAuthorizationRevoked},
		{"API key rotates during fresh account lookup", func(_ *OpenAIGatewayService, _ *StoredResponse, a *native15ParentAccounts, _ *storedNativeAccountAuthorizationGroups) {
			a.before = func(read int) {
				if read == 2 {
					a.current.Credentials["api_key"] = "rotated-during-request"
				}
			}
		}, ErrBPSRequestAuthorizationRevoked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx, record, accounts, groups := native15ParentFixture(t)
			tc.change(svc, record, accounts, groups)
			parent, err := svc.ResolveStoredResponseNativeParent15(ctx, StoredResponseOwner{1, 2, 3}, record.PublicID, "public", "public")
			if !errors.Is(err, tc.want) || parent != nil {
				t.Fatalf("authority drift returned native bytes: %+v %v", parent, err)
			}
		})
	}
}

func TestStoredResponseNativeParent15RechecksAfterAuthorizationWait(t *testing.T) {
	for _, kind := range []string{"request revoked", "account rotates", "resource expires", "resource deleted"} {
		t.Run(kind, func(t *testing.T) {
			svc, _, record, accounts, _ := native15ParentFixture(t)
			calls := 0
			ctx := WithBPSRequestReauthorize(context.Background(), func(context.Context, string) error {
				calls++
				if calls == 2 {
					switch kind {
					case "request revoked":
						return ErrBPSRequestAuthorizationRevoked
					case "account rotates":
						accounts.current.Credentials["bps_responses_credential_generation"] = "next"
					case "resource expires":
						record.ExpiresAt = time.Now().Add(-time.Second)
					case "resource deleted":
						now := time.Now()
						record.DeletedAt = &now
					}
				}
				return nil
			})
			parent, err := svc.ResolveStoredResponseNativeParent15(ctx, StoredResponseOwner{1, 2, 3}, record.PublicID, "public", "public")
			if err == nil || parent != nil || calls < 2 {
				t.Fatalf("post-wait change escaped: %s calls=%d parent=%+v err=%v", kind, calls, parent, err)
			}
		})
	}
}

func TestStoredResponseNativeParent15RequiresCurrentPublicModelAndOwner(t *testing.T) {
	svc, ctx, record, accounts, _ := native15ParentFixture(t)
	for _, tc := range []struct {
		owner StoredResponseOwner
		model string
	}{{StoredResponseOwner{2, 9, 3}, "public"}, {StoredResponseOwner{1, 9, 8}, "public"}, {StoredResponseOwner{1, 2, 3}, "different"}} {
		if parent, err := svc.ResolveStoredResponseNativeParent15(ctx, tc.owner, record.PublicID, tc.model, tc.model); err == nil || parent != nil {
			t.Fatal("cross-owner/group or changed public model authorized")
		}
	}
	accounts.reads = 0
	if parent, err := svc.ResolveStoredResponseNativeParent15(context.Background(), StoredResponseOwner{1, 2, 3}, record.PublicID, "public", "public"); !errors.Is(err, ErrBPSRequestAuthorizationUnavailable) || parent != nil || accounts.reads != 0 {
		t.Fatal("missing current request authorizer reached account/private recovery")
	}
}

func TestStoredResponseNativeParentPlan15ReauthorizesBeforeEverySend(t *testing.T) {
	for _, kind := range []string{"unchanged", "resource deleted", "resource expires", "generation rotates", "API key rotates", "base URL rotates", "service key rotates", "snapshot differs", "owning account differs"} {
		t.Run(kind, func(t *testing.T) {
			svc, ctx, record, accounts, _ := native15ParentFixture(t)
			parent, err := svc.ResolveStoredResponseNativeParent15(ctx, StoredResponseOwner{1, 2, 3}, record.PublicID, "public", "public")
			if err != nil {
				t.Fatal(err)
			}
			plan, err := NewStoredResponseNativePlan15(StoredResponseOwner{1, 2, 3}, []byte(`{"model":"public","input":"next","store":true}`), record.PublicID, parent.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "resource deleted":
				now := time.Now()
				record.DeletedAt = &now
			case "resource expires":
				record.ExpiresAt = time.Now().Add(-time.Second)
			case "generation rotates":
				accounts.current.Credentials["bps_responses_credential_generation"] = "next"
			case "API key rotates":
				accounts.current.Credentials["api_key"] = "next"
			case "base URL rotates":
				accounts.current.Credentials["base_url"] = "https://other.example"
			case "service key rotates":
				accounts.current.Credentials["bps_responses_service_key"] = "23456789012345678901234567890123"
			case "snapshot differs":
				plan.native15.parentSnapshot[0] = 'x'
			case "owning account differs":
				parent.Account.ID++
			}
			err = svc.authorizeStoredResponseNativeParentPlan15(ctx, plan, parent.Account, "public", "public")
			if kind == "unchanged" {
				if err != nil {
					t.Fatalf("unchanged durable parent rejected: %v", err)
				}
			} else if err == nil {
				t.Fatalf("%s escaped final parent authorization", kind)
			}
		})
	}
}

func TestStoredResponseNativeParentPlan15RejectsInPlaceRotationDuringLookup(t *testing.T) {
	svc, ctx, record, accounts, _ := native15ParentFixture(t)
	parent, err := svc.ResolveStoredResponseNativeParent15(ctx, StoredResponseOwner{1, 2, 3}, record.PublicID, "public", "public")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewStoredResponseNativePlan15(StoredResponseOwner{1, 2, 3}, []byte(`{"model":"public","input":"next","store":true}`), record.PublicID, parent.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	// The selected account intentionally shares the repository map here; the
	// helper must freeze it before its fresh resolver performs the first read.
	selected := accounts.current
	next := accounts.reads + 1
	accounts.before = func(read int) {
		if read == next {
			accounts.current.Credentials["api_key"] = "rotated-during-helper"
		}
	}
	if err = svc.authorizeStoredResponseNativeParentPlan15(ctx, plan, selected, "public", "public"); !errors.Is(err, ErrBPSRequestAuthorizationRevoked) {
		t.Fatalf("in-place key rotation escaped helper fence: %v", err)
	}
}

func TestStoredResponseNativeParent15RechecksEarlierRowsAfterLastAncestorWait(t *testing.T) {
	for _, tc := range []struct {
		name          string
		depth, target int
		expire        bool
	}{
		{"two turns leaf deleted", 2, 1, false},
		{"three turns earlier ancestor deleted", 3, 1, false},
		{"two turns leaf expires", 2, 1, true},
		{"three turns earlier ancestor expires", 3, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, records := native15ParentChainFixture(t, tc.depth)
			checks, entered := 0, false
			ctx := WithBPSRequestReauthorize(context.Background(), func(context.Context, string) error {
				checks++
				// One initial check followed by leaf-to-root ancestor checks.
				if checks == tc.depth+1 {
					entered = true
					if tc.expire {
						records[tc.target].ExpiresAt = time.Now().Add(-time.Second)
					} else {
						now := time.Now()
						records[tc.target].DeletedAt = &now
					}
				}
				return nil
			})
			leaf := records[len(records)-1]
			parent, err := svc.ResolveStoredResponseNativeParent15(ctx, StoredResponseOwner{1, 2, 3}, leaf.PublicID, "public", "public")
			if !entered || !errors.Is(err, ErrStoredResponseNotFound) || parent != nil {
				t.Fatalf("earlier resource escaped last ancestor wait: entered=%v checks=%d parent=%+v err=%v", entered, checks, parent, err)
			}
		})
	}
}

func native15ParentChainFixture(t *testing.T, depth int) (*OpenAIGatewayService, context.Context, []*StoredResponse) {
	t.Helper()
	svc, ctx, root, _, _ := native15ParentFixture(t)
	var material storedResponseNative15
	if err := json.Unmarshal(root.Recovery, &material); err != nil {
		t.Fatal(err)
	}
	var rootTurn map[string]any
	if err := json.Unmarshal(material.Snapshot, &rootTurn); err != nil {
		t.Fatal(err)
	}
	turns := []any{rootTurn}
	records := []*StoredResponse{root}
	repo := svc.storedResponses.repo.(*previousResponse15Repo)
	gateway := []byte(`{"object":"response","status":"completed","output":[]}`)
	for index := 2; index <= depth; index++ {
		child := *records[len(records)-1]
		child.PublicID = fmt.Sprintf("resp_local_%032x", index)
		child.PreviousResponseID = records[len(records)-1].PublicID
		child.Binding = fmt.Sprintf("%064x", index)
		text := fmt.Sprintf("followup %d", index)
		child.Input, _ = json.Marshal(text)
		turn := make(map[string]any, len(rootTurn))
		for key, value := range rootTurn {
			turn[key] = value
		}
		turn["input"] = storedNativeInput15(text)
		turns = append(turns, turn)
		snapshot, err := json.Marshal(map[string]any{"version": 2, "binding": rootTurn["binding"], "turns": turns})
		if err != nil {
			t.Fatal(err)
		}
		public, err := json.Marshal(map[string]any{"id": child.PublicID, "object": "response", "created_at": child.CreatedAt.Unix(), "model": "public", "store": true, "status": "completed", "output": []any{}})
		if err != nil {
			t.Fatal(err)
		}
		proof := native15VerifiedFixture(&child, gateway, snapshot)
		proof.subjectDigest, proof.backendID, proof.credentialGeneration = material.SubjectDigest, material.BackendID, material.CredentialGeneration
		child.PublicResponse = public
		child.Recovery, err = BuildStoredResponseNativeRecovery15(&child, public, gateway, snapshot, proof, 16384)
		if err != nil {
			t.Fatal(err)
		}
		repo.records[child.PublicID] = &child
		records = append(records, &child)
	}
	return svc, ctx, records
}

type native15ParentAccounts struct {
	AccountRepository
	current *Account
	reads   int
	before  func(int)
}

func (r *native15ParentAccounts) GetByID(_ context.Context, id int64) (*Account, error) {
	r.reads++
	if r.before != nil {
		r.before(r.reads)
	}
	if r.current == nil || r.current.ID != id {
		return nil, ErrBPSStoredResponseRoute
	}
	return r.current, nil
}

func native15ParentFixture(t *testing.T) (*OpenAIGatewayService, context.Context, *StoredResponse, *native15ParentAccounts, *storedNativeAccountAuthorizationGroups) {
	t.Helper()
	record := native15RecordFixture()
	account := storedResponsesTestAccount(record.AccountID, record.GroupID)
	account.Credentials["bps_responses_service_key"] = "12345678901234567890123456789012"
	account.Credentials["model_mapping"] = map[string]any{"public": "resolved", "forward": "resolved"}
	backend, err := BPSStoredResponsesConfigForAccount(&account)
	if err != nil {
		t.Fatal(err)
	}
	subject := managedfiles.NativeResponsesSubjectDigest15(backend.Expected)
	snapshot, err := json.Marshal(map[string]any{"version": 1, "binding": map[string]any{"owner_id": record.UserID, "group_id": record.GroupID, "account_id": record.AccountID, "model": record.ResolvedModel, "subject_digest": subject}, "input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "question"}}}}, "tools": []any{}, "output": []any{}, "replays": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	public := []byte(`{"id":"resp_local_00000000000000000000000000000001","object":"response","created_at":1,"model":"public","store":true,"status":"completed","output":[]}`)
	gateway := []byte(`{"object":"response","status":"completed","output":[]}`)
	proof := native15VerifiedFixture(record, gateway, snapshot)
	proof.subjectDigest = subject
	proof.backendID = backend.Expected.BackendID
	proof.credentialGeneration = backend.Expected.CredentialGeneration
	record.Recovery, err = BuildStoredResponseNativeRecovery15(record, public, gateway, snapshot, proof, 16384)
	if err != nil {
		t.Fatal(err)
	}
	record.PublicResponse = public
	storageCfg := DefaultStoredResponseConfig()
	storageCfg.Enabled = true
	stored, err := NewStoredResponseService(&previousResponse15Repo{records: map[string]*StoredResponse{record.PublicID: record}}, storageCfg)
	if err != nil {
		t.Fatal(err)
	}
	accounts := &native15ParentAccounts{current: &account}
	groups := &storedNativeAccountAuthorizationGroups{group: &Group{ID: record.GroupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: accounts, schedulerSnapshot: &SchedulerSnapshotService{groupRepo: groups}, storedResponses: stored}
	ctx := WithBPSRequestReauthorize(context.Background(), func(ctx context.Context, model string) error {
		if model != "public" {
			t.Fatal("authorization used forwarded instead of original public model")
		}
		return ctx.Err()
	})
	return svc, ctx, record, accounts, groups
}

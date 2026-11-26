package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type bpsToolProbeAccountRepo struct {
	AccountRepository
	account  *Account
	accounts map[int64]*Account
}

func (r *bpsToolProbeAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.accounts != nil {
		return r.accounts[id], nil
	}
	return r.account, nil
}

type bpsToolProbeUpstream struct {
	HTTPUpstream
	t           *testing.T
	bodies      [][]byte
	accountIDs  []int64
	rejectStage int
	rejectKind  string
}

func (u *bpsToolProbeUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	require.NoError(u.t, err)
	u.bodies = append(u.bodies, body)
	u.accountIDs = append(u.accountIDs, accountID)
	require.Equal(u.t, OpenAIBPSResponsesURL, req.URL.String(), "the probe must never use native Codex")
	require.Equal(u.t, "Bearer test-secret", req.Header.Get("Authorization"))
	require.Equal(u.t, "workspace-1", req.Header.Get("chatgpt-account-id"))
	require.Equal(u.t, "gpt-6-astra", gjson.GetBytes(body, "model").String())
	stage := len(u.bodies)
	headers := http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{fmt.Sprintf("req-probe-%d", stage)}}
	nonce := ""
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("role").String() == "user" {
			for _, part := range item.Get("content").Array() {
				const prefix = "Reply with exactly this nonce and nothing else: "
				if text := part.Get("text").String(); strings.HasPrefix(text, prefix) {
					nonce = strings.TrimPrefix(text, prefix)
				}
			}
		}
	}
	// A non-probe request gets ordinary text, exposing an accidental mode fallback.
	if nonce == "" {
		nonce = "ordinary-text"
	}
	response := bpsResponse(bpsText(nonce))
	if stage == 2 {
		response = bpsResponse(bpsNative("probe_call", "sub2api_probe_echo", map[string]any{"nonce": nonce}))
	}
	if stage == 3 {
		var restoredCall, returnedResult bool
		for _, item := range gjson.GetBytes(body, "input").Array() {
			if item.Get("type").String() == "function_call" && item.Get("name").String() == "run_officejs" && item.Get("call_id").String() == "probe_call" {
				restoredCall = true
			}
			if item.Get("type").String() == "function_call_output" && item.Get("call_id").String() == "probe_call" && item.Get("output").String() == nonce {
				returnedResult = true
			}
		}
		require.True(u.t, restoredCall, "the production adapter must replay the original call from its state store")
		require.True(u.t, returnedResult, "only the fixed nonce echo result may be returned")
	}
	if stage == u.rejectStage {
		switch u.rejectKind {
		case "http_forbidden":
			return &http.Response{StatusCode: 403, Header: headers, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"policy_denied","message":"body-secret test-secret"}}`))}, nil
		case "wrong_text", "wrong_result":
			response = bpsResponse(bpsText("wrong-nonce"))
		case "missing_tool":
			response = bpsResponse(bpsText(nonce))
		case "wrong_arguments":
			response = bpsResponse(bpsNative("probe_call", "sub2api_probe_echo", map[string]any{"nonce": "wrong-nonce"}))
		case "extra_arguments":
			response = bpsResponse(bpsNative("probe_call", "sub2api_probe_echo", map[string]any{"nonce": nonce, "command": "arbitrary-code"}))
		case "wrong_tool":
			response = bpsResponse(bpsNative("probe_call", "other_tool", map[string]any{"nonce": nonce}))
		case "multiple_tools":
			response = bpsResponse(bpsNative("probe_call", "sub2api_probe_echo", map[string]any{"nonce": nonce}), bpsNative("another_call", "sub2api_probe_echo", map[string]any{"nonce": nonce}))
		case "incomplete":
			response["status"] = "incomplete"
		case "interrupted":
			return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(bpsEvent("response.created", map[string]any{"response": response})))}, nil
		case "repeated_tool":
			response = bpsResponse(bpsNative("second_call", "sub2api_probe_echo", map[string]any{"nonce": nonce}))
		}
	}
	return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(bpsEvent("response.completed", map[string]any{"response": response})))}, nil
}

func TestAccountConnectionBPSToolsVerifiesThreeStageProductionAdapterRoundtrip(t *testing.T) {
	gateway, account := bpsFixture()
	account.Credentials["model_mapping"] = map[string]any{"probe-alias": "gpt-6-astra"}
	upstream := &bpsToolProbeUpstream{t: t}
	gateway.httpUpstream = upstream
	svc := &AccountTestService{accountRepo: &bpsToolProbeAccountRepo{account: account}, openaiGatewayService: gateway}
	c, rec := bpsContext(1, "/api/v1/admin/accounts/7/test")

	require.NoError(t, svc.TestAccountConnection(c, account.ID, "probe-alias", "must be ignored", "bps_tools"))
	require.Len(t, upstream.bodies, 3)
	require.Equal(t, []int64{account.ID, account.ID, account.ID}, upstream.accountIDs)
	session := gjson.GetBytes(upstream.bodies[0], "prompt_cache_key").String()
	require.NotEmpty(t, session)
	for _, body := range upstream.bodies[1:] {
		require.Equal(t, session, gjson.GetBytes(body, "prompt_cache_key").String())
	}
	require.Equal(t, gjson.GetBytes(upstream.bodies[1], "metadata.turn_id").String(), gjson.GetBytes(upstream.bodies[2], "metadata.turn_id").String())
	require.Equal(t, "2", gjson.GetBytes(upstream.bodies[2], "metadata.agent_iteration").String())
	var usageCount, verifiedCount, successCount, upstreamCount int
	for _, event := range bpsTestEvents(rec.Body.String()) {
		if event.Get("type").String() == "upstream_response" {
			upstreamCount++
			require.Equal(t, int64(200), event.Get("upstream_status").Int())
			require.Equal(t, "gpt-6-astra", event.Get("upstream_model").String())
			require.Equal(t, fmt.Sprintf("req-probe-%d", upstreamCount), event.Get("request_id").String())
		}
		if event.Get("type").String() == "status" {
			if strings.Contains(event.Get("text").String(), "Tokens: input 100") {
				usageCount++
			}
			if strings.Contains(event.Get("text").String(), "verified") {
				verifiedCount++
			}
		}
		if event.Get("type").String() == "test_complete" && event.Get("success").Bool() {
			successCount++
		}
	}
	require.Equal(t, 3, usageCount, "retain usage for every paid upstream stage")
	require.Equal(t, 3, upstreamCount, "retain actual upstream diagnostics for every stage")
	require.Equal(t, 3, verifiedCount)
	require.Equal(t, 1, successCount)
	require.NotContains(t, rec.Body.String(), "test-secret")
}

func TestAccountConnectionBPSToolsRejectsInvalidOrIncompleteStages(t *testing.T) {
	for _, tc := range []struct {
		stage int
		kind  string
	}{
		{1, "wrong_text"}, {1, "incomplete"}, {1, "interrupted"},
		{2, "missing_tool"}, {2, "wrong_arguments"}, {2, "extra_arguments"}, {2, "wrong_tool"}, {2, "multiple_tools"}, {2, "incomplete"}, {2, "http_forbidden"},
		{3, "wrong_result"}, {3, "repeated_tool"}, {3, "incomplete"}, {3, "interrupted"},
	} {
		t.Run(fmt.Sprintf("%s/stage-%d", tc.kind, tc.stage), func(t *testing.T) {
			gateway, account := bpsFixture()
			upstream := &bpsToolProbeUpstream{t: t, rejectStage: tc.stage, rejectKind: tc.kind}
			gateway.httpUpstream = upstream
			svc := &AccountTestService{accountRepo: &bpsToolProbeAccountRepo{account: account}, openaiGatewayService: gateway}
			c, rec := bpsContext(1, "/api/v1/admin/accounts/7/test")

			require.Error(t, svc.TestAccountConnection(c, account.ID, "gpt-6-astra", "", "bps_tools"))
			require.Len(t, upstream.bodies, tc.stage, "stop before any later stage")
			var errors, upstreamCount int
			for _, event := range bpsTestEvents(rec.Body.String()) {
				require.False(t, event.Get("type").String() == "test_complete" && event.Get("success").Bool())
				if event.Get("type").String() == "upstream_response" {
					upstreamCount++
				}
				if event.Get("type").String() == "error" {
					errors++
					require.True(t, strings.HasPrefix(event.Get("code").String(), "bps_tool_probe_"))
					require.Equal(t, fmt.Sprintf("req-probe-%d", tc.stage), event.Get("request_id").String())
					if tc.kind == "http_forbidden" {
						require.Equal(t, int64(403), event.Get("upstream_status").Int())
						require.Equal(t, "policy_denied", event.Get("upstream_error_code").String())
					}
				}
			}
			require.Equal(t, 1, errors)
			require.Equal(t, tc.stage, upstreamCount)
			require.NotContains(t, rec.Body.String(), "body-secret")
			require.NotContains(t, rec.Body.String(), "test-secret")
		})
	}
}

func TestAccountConnectionBPSToolsRejectsNativeAccountWithoutFallback(t *testing.T) {
	_, account := bpsFixture()
	account.Platform = PlatformOpenAI
	account.Credentials["access_token"] = ""
	svc := &AccountTestService{accountRepo: &bpsToolProbeAccountRepo{account: account}}
	c, rec := bpsContext(1, "/api/v1/admin/accounts/7/test")
	require.Error(t, svc.TestAccountConnection(c, account.ID, "gpt-6-astra", "", "bps_tools"))
	require.Contains(t, rec.Body.String(), "BPS")
}

type bpsToolProbeWaitingUpstream struct {
	HTTPUpstream
	entered chan int64
}

func (u *bpsToolProbeWaitingUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	select {
	case u.entered <- accountID:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestAccountConnectionBPSToolsBoundsConcurrencyAndReleasesOnCancellation(t *testing.T) {
	gateway, account := bpsFixture()
	accounts := make(map[int64]*Account)
	for id := int64(7); id <= 10; id++ {
		copy := *account
		copy.ID = id
		accounts[id] = &copy
	}
	waiting := &bpsToolProbeWaitingUpstream{entered: make(chan int64, 4)}
	gateway.httpUpstream = waiting
	svc := &AccountTestService{accountRepo: &bpsToolProbeAccountRepo{accounts: accounts}, openaiGatewayService: gateway}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for id := int64(7); id <= 9; id++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			c, _ := bpsContext(1, "/api/v1/admin/accounts/test")
			c.Request = c.Request.WithContext(ctx)
			_ = svc.TestAccountConnection(c, id, "gpt-6-astra", "", "bps_tools")
		}(id)
		select {
		case <-waiting.entered:
		case <-ctx.Done():
			t.Fatal("probe did not reach upstream")
		}
	}
	for _, id := range []int64{7, 10} {
		c, rec := bpsContext(1, "/api/v1/admin/accounts/test")
		c.Request = c.Request.WithContext(ctx)
		require.Error(t, svc.TestAccountConnection(c, id, "gpt-6-astra", "", "bps_tools"))
		require.Contains(t, rec.Body.String(), "already running for this account or at capacity")
	}
	cancel()
	wg.Wait()
	upstream := &bpsToolProbeUpstream{t: t}
	gateway.httpUpstream = upstream
	c, _ := bpsContext(1, "/api/v1/admin/accounts/test")
	require.NoError(t, svc.TestAccountConnection(c, 10, "gpt-6-astra", "", "bps_tools"), "cancelled probes must release every slot")
	require.Len(t, upstream.bodies, 3)
}

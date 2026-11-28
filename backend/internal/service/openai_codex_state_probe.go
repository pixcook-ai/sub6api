package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

type OpenAICodexStateVerdict string

const (
	OpenAICodexStateHealthy      OpenAICodexStateVerdict = "healthy"
	OpenAICodexStateDegraded     OpenAICodexStateVerdict = "degraded"
	OpenAICodexStateInconclusive OpenAICodexStateVerdict = "inconclusive"

	openAICodexStateProbeDefaultModel = "gpt-6-astra"
	openAICodexStateProbeShotTimeout  = 45 * time.Second
	openAICodexStateProbeMaxBody      = 1 << 20
)

var ErrOpenAICodexStateProbeBusy = infraerrors.Conflict("STATE_PROBE_BUSY", "A state probe is already running for this account")

type OpenAICodexStateProbeResult struct {
	AccountID            int64                   `json:"account_id"`
	Model                string                  `json:"model"`
	Verdict              OpenAICodexStateVerdict `json:"verdict"`
	Reason               string                  `json:"reason"`
	Failure              string                  `json:"failure,omitempty"`
	MintStatus           int                     `json:"mint_status"`
	ContinueStatus       int                     `json:"continue_status"`
	Minted               bool                    `json:"minted"`
	NewTicket            bool                    `json:"new_ticket"`
	TicketLength         int                     `json:"ticket_length"`
	ContinueTicketLength int                     `json:"continue_ticket_length"`
	ReportedModel        string                  `json:"reported_model,omitempty"`
	LatencyMs            int64                   `json:"latency_ms"`
	StartedAt            time.Time               `json:"started_at"`
	FinishedAt           time.Time               `json:"finished_at"`
}

type openAICodexStateShot struct {
	status           int
	state            string
	cookies          string
	reportedModel    string
	completed        bool
	modelUnsupported bool
}

// ProbeOpenAICodexState keeps both requests on the selected account and proxy.
// The state and routing cookies are confined to the probe.
func (s *AccountTestService) ProbeOpenAICodexState(ctx context.Context, accountID int64, model string) (*OpenAICodexStateProbeResult, error) {
	if s == nil || s.accountRepo == nil {
		return nil, infraerrors.ServiceUnavailable("STATE_PROBE_UNAVAILABLE", "Account testing is unavailable")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, ErrAccountNotFound
	}
	if _, loaded := s.iqStateProbeAccounts.LoadOrStore(accountID, struct{}{}); loaded {
		return nil, ErrOpenAICodexStateProbeBusy
	}
	defer s.iqStateProbeAccounts.Delete(accountID)

	model = strings.TrimSpace(model)
	if model == "" {
		model = openAICodexStateProbeDefaultModel
	}
	result := &OpenAICodexStateProbeResult{
		AccountID: accountID,
		Model:     normalizeOpenAIModelForUpstream(account, account.GetMappedModel(model)),
		Verdict:   OpenAICodexStateInconclusive,
		StartedAt: time.Now(),
	}
	defer func() {
		result.FinishedAt = time.Now()
		result.LatencyMs = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
	}()

	if !account.IsOpenAIOAuthLike() || account.IsSyntheticUITest() || account.IsOpenAIAgentIdentity() {
		result.fail("unsupported", "探针仅支持使用 Codex 门票的 OpenAI 订阅账号")
		return result, nil
	}
	gateway := s.openaiGatewayService
	if gateway == nil || gateway.httpUpstream == nil {
		result.fail("unsupported", "OpenAI 网关不可用")
		return result, nil
	}
	proxyURL := upstreamModelsProxyURL(account)
	if account.ProxyID != nil && proxyURL == "" {
		result.fail("network_error", "账号代理不可用")
		return result, nil
	}
	credential, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil || credential == nil {
		result.fail("account_error", "无法读取账号凭据")
		return result, nil
	}
	if credential.IsOpenAIAgentIdentity() {
		result.fail("unsupported", "该账号使用 Agent Identity")
		return result, nil
	}
	token, _, err := gateway.GetAccessToken(ctx, credential)
	if err != nil || strings.TrimSpace(token) == "" {
		result.fail("account_error", "无法取得账号访问令牌")
		return result, nil
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Accept", "text/event-stream")
	headers.Set("Content-Type", "application/json")
	setOpenAIChatGPTAccountHeaders(headers, credential)
	applyOpenAICodexTicketHarvestIdentity(headers, result.Model)

	mint, err := gateway.fireOpenAICodexStateShot(ctx, account, headers, result.Model, proxyURL, "", "")
	result.MintStatus = mint.status
	result.TicketLength = len(mint.state)
	if !result.shotUsable(ctx, "打票", mint, err) {
		return result, nil
	}
	if mint.state == "" {
		result.fail("no_ticket", "上游未返回门票，无法进行续接探测")
		return result, nil
	}
	result.Minted = true
	continuation, err := gateway.fireOpenAICodexStateShot(ctx, account, headers, result.Model, proxyURL, mint.state, mint.cookies)
	result.ContinueStatus = continuation.status
	result.ContinueTicketLength = len(continuation.state)
	if !result.shotUsable(ctx, "门票续接", continuation, err) {
		return result, nil
	}
	result.ReportedModel = continuation.reportedModel
	if result.ReportedModel == "" {
		result.ReportedModel = mint.reportedModel
	}
	result.NewTicket = continuation.state != "" && continuation.state != mint.state
	if result.NewTicket {
		result.Verdict = OpenAICodexStateDegraded
		result.Reason = "上游在续接时签发新门票，符合降智特征"
	} else {
		result.Verdict = OpenAICodexStateHealthy
		result.Reason = "上游在续接时沿用原门票，本次探测正常"
	}
	return result, nil
}

func (r *OpenAICodexStateProbeResult) fail(failure, reason string) {
	r.Failure = failure
	r.Reason = reason
}

func (r *OpenAICodexStateProbeResult) shotUsable(ctx context.Context, step string, shot openAICodexStateShot, err error) bool {
	switch {
	case ctx.Err() != nil:
		r.fail("cancelled", step+"已取消或超时")
	case err != nil:
		r.fail("network_error", step+"请求失败或超时")
	case shot.status == http.StatusOK && shot.completed:
		return true
	case shot.status == http.StatusOK:
		r.fail("stream_error", step+"响应流未正常完成")
	case shot.status == http.StatusUnauthorized || shot.status == http.StatusForbidden:
		r.fail("account_error", fmt.Sprintf("%s被上游拒绝（HTTP %d）", step, shot.status))
	case shot.status == http.StatusTooManyRequests:
		r.fail("rate_limited", step+"被上游限流（HTTP 429）")
	case shot.modelUnsupported:
		r.fail("model_unsupported", fmt.Sprintf("上游不支持此模型（HTTP %d）", shot.status))
	default:
		r.fail("upstream_error", fmt.Sprintf("%s返回异常状态（HTTP %d）", step, shot.status))
	}
	return false
}

func (s *OpenAIGatewayService) fireOpenAICodexStateShot(ctx context.Context, account *Account, headers http.Header, model, proxy, state, cookies string) (openAICodexStateShot, error) {
	var shot openAICodexStateShot
	ctx, cancel := context.WithTimeout(ctx, openAICodexStateProbeShotTimeout)
	defer cancel()

	body := []byte(`{"model":` + jsonString(model) + `,"instructions":"Reply with OK.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply with OK."}]}],"stream":true,"store":false,"parallel_tool_calls":true,"include":["reasoning.encrypted_content"]}`)
	req, err := http.NewRequestWithContext(WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileOpenAIHarvest), http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
	if err != nil {
		return shot, err
	}
	req.Close = true
	req.Header = headers.Clone()
	req.Header.Set("session_id", uuid.NewString())
	if state != "" {
		req.Header.Set(openAICodexTurnStateHeader, state)
	}
	if cookies != "" {
		req.Header.Set("Cookie", cookies)
	}

	resp, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		return shot, err
	}
	if resp == nil {
		return shot, fmt.Errorf("missing upstream response")
	}
	shot.status = resp.StatusCode
	shot.state = extractOpenAICodexTurnState(resp.Header)
	seen := make(map[string]bool, 2)
	var cookiesForContinuation []string
	for _, cookie := range resp.Cookies() {
		if (cookie.Name != "__cflb" && cookie.Name != "__oailb") || cookie.Value == "" || cookie.MaxAge < 0 || seen[cookie.Name] {
			continue
		}
		seen[cookie.Name] = true
		cookiesForContinuation = append(cookiesForContinuation, (&http.Cookie{Name: cookie.Name, Value: cookie.Value, Quoted: cookie.Quoted}).String())
	}
	shot.cookies = strings.Join(cookiesForContinuation, "; ")
	if resp.Body == nil {
		return shot, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, openAICodexStateProbeMaxBody+1))
	if err != nil || len(data) > openAICodexStateProbeMaxBody {
		return shot, ctx.Err()
	}
	if shot.status != http.StatusOK {
		message := strings.ToLower(extractUpstreamErrorMessage(data))
		shot.modelUnsupported = (shot.status == http.StatusBadRequest || shot.status == http.StatusNotFound) &&
			(extractUpstreamErrorCode(data) == "model_not_found" || strings.Contains(message, "not supported"))
		return shot, nil
	}
	shot.completed, shot.reportedModel = validCodexStateProbeCompletion(data)
	return shot, nil
}

// Require a terminal success event, even when the upstream returns HTTP 200.
func validCodexStateProbeCompletion(data []byte) (bool, string) {
	body := string(data)
	if !strings.HasSuffix(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n") {
		return false, ""
	}
	var completed, failed bool
	var model string
	forEachOpenAISSEFrame(body, func(eventType string, payload []byte) {
		if !gjson.ValidBytes(payload) {
			failed = true
			return
		}
		event := gjson.ParseBytes(payload)
		if status := event.Get("response.status").String(); status == "failed" || status == "incomplete" || status == "cancelled" || status == "canceled" {
			failed = true
		}
		switch eventType {
		case "error", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
			failed = true
		case "response.completed":
			completed = event.Get("response.status").String() == "completed"
		}
		if eventType == "response.created" || eventType == "response.completed" {
			if reported := event.Get("response.model").String(); reported != "" {
				model = reported
			}
		}
	})
	return completed && !failed, model
}

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
	"github.com/MACOS-DO/sub4api/internal/pkg/modeltrace"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
)

type modelTraceEnvironment struct {
	Protocol     string `json:"protocol"`
	SystemPrompt string `json:"system_prompt"`
	OAuth        bool   `json:"oauth"`
}

type ModelTraceSample struct {
	Challenge      modeltrace.Challenge  `json:"challenge"`
	Text           string                `json:"text"`
	MappedModel    string                `json:"mapped_model"`
	Environment    modelTraceEnvironment `json:"environment"`
	ParsedNumbers  int                   `json:"parsed_numbers"`
	MinimumNumbers int                   `json:"minimum_numbers"`
	Accepted       bool                  `json:"accepted"`
	Error          string                `json:"error,omitempty"`
	Analysis       *modeltrace.Analysis  `json:"analysis,omitempty"`
	StartedAt      time.Time             `json:"started_at"`
	LatencyMs      int64                 `json:"latency_ms"`
}

type ModelTraceResult struct {
	RequestedModel  string               `json:"requested_model"`
	ReasoningEffort string               `json:"reasoning_effort"`
	Bank            modeltrace.Metadata  `json:"bank"`
	Samples         []ModelTraceSample   `json:"samples"`
	Analysis        *modeltrace.Analysis `json:"analysis,omitempty"`
	Status          string               `json:"status"`
	Disagreement    bool                 `json:"disagreement"`
	CreatedAt       time.Time            `json:"created_at"`
}

func (s *AccountTestService) modelTraceEnvironmentForAccount(account *Account, model string) (modelTraceEnvironment, error) {
	unsupported := func() (modelTraceEnvironment, error) {
		return modelTraceEnvironment{}, infraerrors.BadRequest("MODELTRACE_UNSUPPORTED", "ModelTrace supports native OpenAI and Anthropic text accounts")
	}
	if account == nil || account.IsSyntheticUITest() || account.IsCredentialShadow() || account.IsBedrock() || (account.Type != AccountTypeAPIKey && !account.IsOAuth()) {
		return unsupported()
	}
	environment := modelTraceEnvironment{OAuth: account.IsOAuth(), SystemPrompt: "none"}
	switch account.Platform {
	case PlatformOpenAI:
		if account.IsOpenAIAgentIdentity() || s.pluginManager.ShouldRouteOpenAIOAuth(account) || isOpenAIImageModel(account.GetMappedModel(model)) {
			return unsupported()
		}
		environment.Protocol = "responses"
		if account.IsOAuth() {
			environment.SystemPrompt = "required_responses_instructions"
		}
		if account.Type == AccountTypeAPIKey && !openai_compat.ShouldUseResponsesAPI(account.Extra) {
			environment.Protocol = "chat_completions"
			environment.SystemPrompt = "none"
		}
	case PlatformAnthropic:
		environment.Protocol = "messages"
		if account.IsOAuth() {
			environment.SystemPrompt = "required_claude_code"
		}
	default:
		return unsupported()
	}
	return environment, nil
}

// TestModelTraceAccountConnection runs exactly three independent prompts, with
// no attribution-driven retries, failover, recovery or scheduling decisions.
// Validation happens before committing SSE headers so normal HTTP errors work.
func (s *AccountTestService) TestModelTraceAccountConnection(c *gin.Context, accountID int64, model, effort string) error {
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		return infraerrors.ServiceUnavailable("MODELTRACE_UNAVAILABLE", "ModelTrace testing is unavailable")
	}
	account, err := s.accountRepo.GetByID(c.Request.Context(), accountID)
	if err != nil {
		return err
	}
	if _, err := s.modelTraceEnvironmentForAccount(account, model); err != nil {
		return err
	}
	if _, loaded := s.modelTraceAccounts.LoadOrStore(accountID, struct{}{}); loaded {
		return infraerrors.Conflict("MODELTRACE_BUSY", "A ModelTrace test is already running for this account")
	}
	defer s.modelTraceAccounts.Delete(accountID)
	challenges, err := modeltrace.Challenges()
	if err != nil {
		return err
	}
	bank, err := modeltrace.BankMetadata()
	if err != nil {
		return err
	}
	result := ModelTraceResult{RequestedModel: model, ReasoningEffort: effort, Bank: bank, CreatedAt: time.Now(), Status: "insufficient"}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	s.sendEvent(c, TestEvent{Type: "modeltrace_start", Data: gin.H{"challenges": challenges, "bank": bank}})
	err = runModelTraceSamples(c.Request.Context(), challenges, func(ctx context.Context, challenge modeltrace.Challenge) ModelTraceSample {
		stop := startOpenAISSEKeepalive(c, 10*time.Second)
		defer stop()
		return s.runModelTraceSample(ctx, accountID, model, effort, challenge)
	}, func(sample ModelTraceSample) {
		result.Samples = append(result.Samples, sample)
		s.sendEvent(c, TestEvent{Type: "modeltrace_sample", Data: sample})
	})
	if err != nil {
		return err
	}
	completeModelTraceResult(&result)
	s.sendEvent(c, TestEvent{Type: "modeltrace_complete", Data: result, Success: true})
	return c.Request.Context().Err()
}

func runModelTraceSamples(ctx context.Context, challenges []modeltrace.Challenge, run func(context.Context, modeltrace.Challenge) ModelTraceSample, emit func(ModelTraceSample)) error {
	for _, challenge := range challenges {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The total round is bounded by three per-sample deadlines. Each sample
		// completes before the next one starts, including releasing its body.
		sampleCtx, cancel := context.WithTimeout(ctx, 240*time.Second)
		sample := run(sampleCtx, challenge)
		cancel()
		if err := ctx.Err(); err != nil {
			return err
		}
		emit(sample)
	}
	return nil
}

func completeModelTraceResult(result *ModelTraceResult) {
	var outputs []modeltrace.Output
	predictions := make(map[string]bool)
	for _, sample := range result.Samples {
		if sample.Accepted {
			outputs = append(outputs, modeltrace.Output{Text: sample.Text, ExpectedCount: sample.Challenge.ExpectedCount})
			if sample.Analysis != nil {
				predictions[sample.Analysis.Prediction] = true
			}
		}
	}
	result.Analysis, _ = modeltrace.Analyze(outputs)
	result.Status = "insufficient"
	if result.Analysis != nil {
		result.Status = "partial"
		if result.Analysis.UsedOutputs == 3 {
			result.Status = "complete"
		}
	}
	result.Disagreement = len(predictions) > 1
}

func (s *AccountTestService) runModelTraceSample(ctx context.Context, accountID int64, model, effort string, challenge modeltrace.Challenge) (sample ModelTraceSample) {
	started := time.Now()
	sample = ModelTraceSample{Challenge: challenge, StartedAt: started, MinimumNumbers: modeltrace.MinimumNumbers(challenge.ExpectedCount)}
	defer func() { sample.LatencyMs = time.Since(started).Milliseconds() }()
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		sample.Error = "account_unavailable"
		return sample
	}
	environment, err := s.modelTraceEnvironmentForAccount(account, model)
	if err != nil {
		sample.Error = "unsupported"
		return sample
	}
	sample.Environment = environment
	ctx = withIQQuestionOptions(ctx, iqQuestionOptions{prompt: challenge.Prompt, reasoningEffort: effort, fingerprint: true})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &modelTraceRecorder{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	c, _ := gin.CreateTestContext(w)
	c.Request = (&http.Request{Header: make(http.Header)}).WithContext(ctx)
	if account.IsAnthropic() {
		err = s.testClaudeAccountConnection(c, account, model)
	} else {
		err = s.testOpenAIAccountConnection(c, account, model, challenge.Prompt, AccountTestModeDefault)
	}
	if w.overflow {
		sample.Error = "output_limit"
		return sample
	}
	var output strings.Builder
	completed := false
	failed := false
	for _, line := range strings.Split(w.Body.String(), "\n") {
		var event TestEvent
		data, ok := strings.CutPrefix(line, "data:")
		if !ok || json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		switch event.Type {
		case "test_start":
			sample.MappedModel = event.Model
			if account.IsOpenAI() && environment.OAuth {
				sample.MappedModel = normalizeOpenAIModelForUpstream(account, event.Model)
			}
		case "test_complete":
			completed = event.Success
		case "content":
			if output.Len()+len(event.Text) > 64<<10 {
				sample.Error = "output_limit"
				return sample
			}
			output.WriteString(event.Text)
		case "error":
			failed = true
		}
	}
	// Only normal completion is eligible for the aggregate. Keep partial text
	// available to inspect, without including error responses from the provider.
	sample.Text = output.String()
	sample.ParsedNumbers = len(modeltrace.ParseNumbers(sample.Text))
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		sample.Error = "timeout"
	case err != nil || failed || !completed:
		// Do not persist provider error bodies, which may echo credentials.
		sample.Error = "generation_failed"
	case sample.ParsedNumbers < sample.MinimumNumbers:
		sample.Error = "too_few_numbers"
	default:
		sample.Analysis, err = modeltrace.Analyze([]modeltrace.Output{{Text: sample.Text, ExpectedCount: challenge.ExpectedCount}})
		if err != nil {
			sample.Error = "analysis_failed"
		} else {
			sample.Accepted = true
		}
	}
	return sample
}

type modelTraceRecorder struct {
	*httptest.ResponseRecorder
	cancel   context.CancelFunc
	overflow bool
}

func (w *modelTraceRecorder) Write(data []byte) (int, error) {
	// The inner SSE adds framing per delta. Permit overhead beyond the text limit.
	if w.Body.Len()+len(data) > 1<<20 {
		w.overflow = true
		w.cancel()
		return 0, errors.New("ModelTrace output exceeds capture limit")
	}
	return w.ResponseRecorder.Write(data)
}

func (w *modelTraceRecorder) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

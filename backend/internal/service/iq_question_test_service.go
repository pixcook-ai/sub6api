package service

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/claude"
	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
)

type iqQuestionOptionsKey struct{}

type iqQuestionOptions struct {
	prompt          string
	reasoningEffort string
	fingerprint     bool
}

func withIQQuestionOptions(ctx context.Context, options iqQuestionOptions) context.Context {
	return context.WithValue(ctx, iqQuestionOptionsKey{}, options)
}

func iqQuestionOptionsFromContext(ctx context.Context) (iqQuestionOptions, bool) {
	options, ok := ctx.Value(iqQuestionOptionsKey{}).(iqQuestionOptions)
	return options, ok
}

func ValidIQReasoningEffort(effort string) bool {
	switch effort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	default:
		return false
	}
}

func (s *AccountTestService) TestPelicanAccountConnection(c *gin.Context, accountID int64, model, prompt, effort string) error {
	ctx := c.Request.Context()
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	if account == nil {
		return ErrAccountNotFound
	}
	mappedModel := account.GetMappedModel(model)
	if (account.IsOpenAI() && isOpenAIImageModel(mappedModel)) ||
		(account.IsGemini() && isImageGenerationModel(mappedModel)) ||
		(account.Platform == PlatformGrok && (isGrokImageGenerationModel(mappedModel) || isGrokVideoGenerationModel(mappedModel))) {
		return infraerrors.BadRequest("IQ_TEST_UNSUPPORTED", "IQ tests require a text model")
	}
	release, err := s.beginIQQuestion(accountID)
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	options, _ := iqQuestionOptionsFromContext(ctx)
	options.prompt = prompt
	options.reasoningEffort = strings.TrimSpace(effort)
	c.Request = c.Request.WithContext(withIQQuestionOptions(ctx, options))
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	return s.testAccountConnection(c, account, model, prompt, AccountTestModeDefault, AccountTestOptions{})
}

func (s *AccountTestService) beginIQQuestion(accountID int64) (func(), error) {
	s.iqQuestionMu.Lock()
	defer s.iqQuestionMu.Unlock()
	if s.iqQuestionRunning[accountID] >= 8 {
		return nil, infraerrors.Conflict("IQ_TEST_BUSY", "This account already has eight running question tests")
	}
	if s.iqQuestionRunning == nil {
		s.iqQuestionRunning = make(map[int64]int)
	}
	s.iqQuestionRunning[accountID]++
	return func() {
		s.iqQuestionMu.Lock()
		defer s.iqQuestionMu.Unlock()
		s.iqQuestionRunning[accountID]--
		if s.iqQuestionRunning[accountID] == 0 {
			delete(s.iqQuestionRunning, accountID)
		}
	}, nil
}

func applyIQClaudePayload(payload map[string]any, model string, options iqQuestionOptions, oauth bool) {
	payload["max_tokens"] = 32000
	if slices.Contains(claude.EffortLevelsForModel(model), options.reasoningEffort) {
		payload["output_config"] = map[string]any{"effort": options.reasoningEffort}
		if !strings.HasPrefix(model, "claude-opus-4-5") {
			payload["thinking"] = map[string]any{"type": "adaptive"}
		}
	}
	if messages, ok := payload["messages"].([]map[string]any); ok && len(messages) > 0 {
		if content, ok := messages[0]["content"].([]map[string]any); ok && len(content) > 0 {
			content[0]["text"] = options.prompt
		}
	}
	if options.fingerprint {
		payload["max_tokens"] = 4096
		delete(payload, "tools")
		delete(payload, "tool_choice")
		delete(payload, "temperature")
		if !oauth {
			delete(payload, "system")
		}
	}
}

func applyIQResponsesPayload(payload map[string]any, model string, options iqQuestionOptions, oauth bool) {
	if input, ok := payload["input"].([]map[string]any); ok && len(input) > 0 {
		if content, ok := input[0]["content"].([]map[string]any); ok && len(content) > 0 {
			content[0]["text"] = options.prompt
		}
	}
	if effort := iqOpenAIReasoningEffort(options.reasoningEffort, model); effort != "" {
		payload["reasoning"] = map[string]any{"effort": effort}
	}
	if options.fingerprint && !oauth {
		delete(payload, "instructions")
		payload["max_output_tokens"] = 4096
	}
}

func iqOpenAIReasoningEffort(effort, model string) string {
	if effort == "ultra" {
		return effort
	}
	return normalizeOpenAIReasoningEffortForModel(effort, model)
}

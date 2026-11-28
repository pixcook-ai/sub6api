package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

const iqHistoryRetention = 7 * 24 * time.Hour
const iqScheduledCaptureLimit = 4 << 20
const iqScheduledAnswerLimit = 2 << 20
const CandyPrompt = `在一个黑色的袋子里放有三种口味的糖果，每种糖果有两种不同的形状（圆形和五角星形，不同的形状靠手感可以分辨）。现已知不同口味的糖和不同形状的数量统计如下表。参赛者需要在活动前决定摸出的糖果数目，那么，最少取出多少个糖果才能保证手中同时拥有不同形状的苹果味和桃子味的糖？（同时手中有圆形苹果味匹配五角星桃子味糖果，或者有圆形桃子味匹配五角星苹果味糖果都满足要求）
苹果味 桃子味 西瓜味
圆形 7 9 8
五角星形 7 6 4`

var iqHTMLPattern = regexp.MustCompile(`(?i)<(?:!doctype\s+html|html|svg)[\s>]`)

func (s *ScheduledTestRunnerService) runIQPlan(ctx context.Context, plan *ScheduledTestPlan) {
	repo, ok := s.planRepo.(scheduledIQPlanRepository)
	if !ok {
		return
	}
	now := time.Now()
	leaseUntil := now.Add(6 * time.Minute).Truncate(time.Microsecond)
	claimed, err := repo.ClaimIQPlan(ctx, plan.ID, plan.UpdatedAt, now, leaseUntil)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "IQ plan %d claim failed: %v", plan.ID, err)
	}
	if err != nil || !claimed {
		return
	}

	count := max(1, min(8, plan.PelicanConfig.ParallelCount))
	if plan.PelicanConfig.QuestionKind == "state_probe" {
		count = 1
	}
	results := make([]*ScheduledTestResult, count)
	var workers sync.WaitGroup
	for i := range results {
		workers.Go(func() {
			results[i] = s.accountTestSvc.runScheduledIQSample(ctx, plan)
		})
	}
	workers.Wait()

	finishedAt := time.Now()
	nextRun, err := computeNextRun(plan.CronExpression, finishedAt)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "IQ plan %d has invalid schedule: %v", plan.ID, err)
		return
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	autoRecover, err := repo.FinishIQPlan(saveCtx, plan, leaseUntil, finishedAt, nextRun, results)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "IQ plan %d save failed: %v", plan.ID, err)
		return
	}
	if autoRecover && !isScheduledIQCandy(plan.PelicanConfig) && plan.PelicanConfig.QuestionKind != "state_probe" {
		for _, result := range results {
			if result.Status == "success" {
				s.tryRecoverAccount(saveCtx, plan.AccountID, plan.ID)
				break
			}
		}
	}
}

func isScheduledIQCandy(cfg *PelicanTestConfig) bool {
	return cfg.QuestionKind == "candy" || strings.TrimSpace(cfg.Prompt) == CandyPrompt
}

func (s *AccountTestService) runScheduledIQSample(ctx context.Context, plan *ScheduledTestPlan) (result *ScheduledTestResult) {
	snapshot := *plan.PelicanConfig
	snapshot.ModelID = plan.ModelID
	builtinCandy := strings.TrimSpace(snapshot.Prompt) == CandyPrompt
	if builtinCandy && snapshot.QuestionKind != "state_probe" {
		snapshot.QuestionKind = "candy"
	}
	started := time.Now()
	result = &ScheduledTestResult{PelicanConfig: &snapshot, StartedAt: started, Status: "failed"}
	defer func() {
		result.FinishedAt = time.Now()
		result.LatencyMs = result.FinishedAt.Sub(started).Milliseconds()
	}()
	if snapshot.QuestionKind == "state_probe" {
		probe, err := s.ProbeOpenAICodexState(ctx, plan.AccountID, plan.ModelID)
		if err != nil {
			result.ErrorMessage = "state_probe_inconclusive: request_failed"
			if errors.Is(err, ErrOpenAICodexStateProbeBusy) || ctx.Err() != nil {
				result.ErrorMessage = "state_probe_inconclusive: cancelled"
			}
			return result
		}
		snapshot.ModelID = probe.Model
		result.ResponseText = fmt.Sprintf("%s\n%s", probe.Verdict, probe.Reason)
		switch string(probe.Verdict) {
		case "healthy":
			result.Status = "success"
		case "degraded":
			result.ErrorMessage = "state_degraded"
		default:
			result.ErrorMessage = "state_probe_inconclusive: " + probe.Failure
		}
		return result
	}

	prompt := snapshot.Prompt + "\n\n只输出独立 HTML，不要使用 Markdown 代码块、外部依赖或解释。"
	if snapshot.QuestionKind == "candy" {
		prompt = snapshot.Prompt + "\n\n只输出最终整数，不要解释。"
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	recorder := &iqScheduledRecorder{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	c, _ := gin.CreateTestContext(recorder)
	c.Request = (&http.Request{Header: make(http.Header)}).WithContext(ctx)
	err := s.TestPelicanAccountConnection(c, plan.AccountID, plan.ModelID, prompt, snapshot.ReasoningEffort)
	output, completed := parseScheduledIQOutput(recorder.Body.String())
	switch {
	case recorder.overflow || len(output) > iqScheduledAnswerLimit:
		result.ErrorMessage = "Response exceeds capture limit"
	case err != nil || !completed:
		result.ErrorMessage = "Generation did not complete successfully"
	case strings.TrimSpace(output) == "":
		result.ErrorMessage = "Model returned empty output"
	case builtinCandy && !CandyAnswerCorrect(output):
		result.ErrorMessage = "answer_mismatch: expected 21"
	case snapshot.QuestionKind != "candy" && !iqHTMLPattern.MatchString(output):
		result.ErrorMessage = "Model did not return HTML or SVG"
	default:
		result.Status = "success"
	}
	if len(output) <= iqScheduledAnswerLimit && !recorder.overflow {
		result.ResponseText = strings.ReplaceAll(output, "\x00", "")
	}
	return result
}

type iqScheduledRecorder struct {
	*httptest.ResponseRecorder
	cancel   context.CancelFunc
	overflow bool
}

func (w *iqScheduledRecorder) Write(data []byte) (int, error) {
	if w.overflow || w.Body.Len()+len(data) > iqScheduledCaptureLimit {
		w.overflow = true
		w.cancel()
		return 0, io.ErrShortWrite
	}
	return w.ResponseRecorder.Write(data)
}

func (w *iqScheduledRecorder) WriteString(data string) (int, error) { return w.Write([]byte(data)) }

func parseScheduledIQOutput(body string) (string, bool) {
	output, message := parseTestSSEOutput(body)
	if message != "" {
		return output, false
	}
	for line := range strings.Lines(body) {
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var event TestEvent
		if json.Unmarshal([]byte(data), &event) == nil && event.Type == "test_complete" {
			return output, event.Success
		}
	}
	return output, false
}

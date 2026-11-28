package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

var scheduledTestCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ScheduledTestService provides CRUD operations for scheduled test plans and results.
type ScheduledTestService struct {
	planRepo   ScheduledTestPlanRepository
	resultRepo ScheduledTestResultRepository
}

// NewScheduledTestService creates a new ScheduledTestService.
func NewScheduledTestService(
	planRepo ScheduledTestPlanRepository,
	resultRepo ScheduledTestResultRepository,
) *ScheduledTestService {
	return &ScheduledTestService{
		planRepo:   planRepo,
		resultRepo: resultRepo,
	}
}

// CreatePlan validates the cron expression, computes next_run_at, and persists the plan.
func (s *ScheduledTestService) CreatePlan(ctx context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	if err := normalizeScheduledIQPlan(plan); err != nil {
		return nil, err
	}
	nextRun, err := computeNextRun(plan.CronExpression, time.Now())
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression: %w", err)
	}
	plan.NextRunAt = &nextRun

	if plan.MaxResults <= 0 {
		plan.MaxResults = 50
	}

	return s.planRepo.Create(ctx, plan)
}

// GetPlan retrieves a plan by ID.
func (s *ScheduledTestService) GetPlan(ctx context.Context, id int64) (*ScheduledTestPlan, error) {
	return s.planRepo.GetByID(ctx, id)
}

// ListPlansByAccount returns all plans for a given account.
func (s *ScheduledTestService) ListPlansByAccount(ctx context.Context, accountID int64) ([]*ScheduledTestPlan, error) {
	return s.planRepo.ListByAccountID(ctx, accountID)
}

// UpdatePlan validates cron and updates the plan.
func (s *ScheduledTestService) UpdatePlan(ctx context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	if err := normalizeScheduledIQPlan(plan); err != nil {
		return nil, err
	}
	nextRun, err := computeNextRun(plan.CronExpression, time.Now())
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression: %w", err)
	}
	plan.NextRunAt = &nextRun

	return s.planRepo.Update(ctx, plan)
}

func normalizeScheduledIQPlan(plan *ScheduledTestPlan) error {
	cfg := plan.PelicanConfig
	if cfg == nil {
		return nil
	}
	switch cfg.QuestionKind {
	case "", "pelican", "candy", "state_probe":
	default:
		return fmt.Errorf("invalid question kind")
	}
	if cfg.QuestionKind != "state_probe" && strings.TrimSpace(cfg.Prompt) == "" {
		return fmt.Errorf("test prompt is required")
	}
	plan.ModelID = strings.TrimSpace(plan.ModelID)
	if len(plan.ModelID) > 100 || (plan.ModelID == "" && cfg.QuestionKind != "state_probe") {
		return fmt.Errorf("invalid model ID")
	}
	if strings.ContainsRune(cfg.Prompt, 0) || len(cfg.Prompt) > 64<<10 {
		return fmt.Errorf("invalid test prompt")
	}
	if !ValidIQReasoningEffort(cfg.ReasoningEffort) {
		return fmt.Errorf("invalid reasoning effort")
	}
	if cfg.ParallelCount == 0 {
		cfg.ParallelCount = 1
	}
	if cfg.ParallelCount < 1 || cfg.ParallelCount > 8 {
		return fmt.Errorf("parallel count must be between 1 and 8")
	}
	if plan.MaxResults <= 0 {
		plan.MaxResults = 100
	}
	if plan.MaxResults > 200 {
		return fmt.Errorf("maximum retained results must not exceed 200")
	}
	cfg.ModelID = ""
	return nil
}

// DeletePlan removes a plan and its results (via CASCADE).
func (s *ScheduledTestService) DeletePlan(ctx context.Context, id int64) error {
	return s.planRepo.Delete(ctx, id)
}

// ListResults returns the most recent results for a plan.
func (s *ScheduledTestService) ListResults(ctx context.Context, planID int64, limit int) ([]*ScheduledTestResult, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.resultRepo.ListByPlanID(ctx, planID, limit)
}

func (s *ScheduledTestService) ListResultSummaries(ctx context.Context, planID int64, limit int) ([]*ScheduledTestResult, error) {
	if repo, ok := s.resultRepo.(scheduledIQResultRepository); ok {
		return repo.ListResultSummaries(ctx, planID, limit)
	}
	return s.ListResults(ctx, planID, limit)
}

func (s *ScheduledTestService) GetResult(ctx context.Context, planID, resultID int64) (*ScheduledTestResult, error) {
	repo, ok := s.resultRepo.(scheduledIQResultRepository)
	if !ok {
		return nil, fmt.Errorf("scheduled result detail unavailable")
	}
	return repo.GetResult(ctx, planID, resultID)
}

func (s *ScheduledTestService) ListPelicanHistory(ctx context.Context, beforeID, accountID int64) (*PelicanHistoryPage, error) {
	repo, ok := s.resultRepo.(scheduledIQResultRepository)
	if !ok {
		return nil, fmt.Errorf("IQ test history unavailable")
	}
	const limit = 100
	items, err := repo.ListPelicanHistory(ctx, beforeID, limit+1, accountID)
	if err != nil {
		return nil, err
	}
	page := &PelicanHistoryPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = items[limit-1].ID
	}
	return page, nil
}

// SaveResult inserts a result and prunes old entries beyond maxResults.
func (s *ScheduledTestService) SaveResult(ctx context.Context, planID int64, maxResults int, result *ScheduledTestResult) error {
	result.PlanID = planID
	if _, err := s.resultRepo.Create(ctx, result); err != nil {
		return err
	}
	return s.resultRepo.PruneOldResults(ctx, planID, maxResults)
}

func computeNextRun(cronExpr string, from time.Time) (time.Time, error) {
	sched, err := scheduledTestCronParser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(from), nil
}

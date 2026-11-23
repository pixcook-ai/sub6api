//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
)

func TestStoredResponseForwardFailureReturnsUsageOnceWithoutHealthPenalty(t *testing.T) {
	upstream := &openAIRecoveryUpstream{}
	sends := 0
	upstream.do = func(*http.Request, string, int64, int) (*http.Response, error) {
		sends++
		return nil, &storedResponseUsageError{err: &StoredResponseLocalError{ErrStoredResponseInvalid}, usage: OpenAIUsage{InputTokens: 7, OutputTokens: 2}}
	}
	repo := &openaiTransportAccountRepoStub{}
	svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream, cfg: &config.Config{}}
	account := &Account{ID: 93, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "synthetic-key", "base_url": "https://synthetic.example/v1"}}
	c, recorder := newOpenAITransportErrTestContext()
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.1","input":"synthetic","stream":false}`))
	var local *StoredResponseLocalError
	var failover *UpstreamFailoverError
	if !errors.As(err, &local) || errors.As(err, &failover) || result == nil || result.Usage.InputTokens != 7 || result.Usage.OutputTokens != 2 || sends != 1 {
		t.Fatalf("failure settlement/replay: sends=%d result=%+v err=%v", sends, result, err)
	}
	if len(repo.tempUnschedCalls) != 0 || recorder.Body.Len() != 0 || svc.isOpenAIAccountRuntimeBlocked(account) {
		t.Fatal("local failure penalized account or wrote an unsafe response")
	}
}

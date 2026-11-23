package service

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	request, coreBinding, coreErr := s.prepareCoreOAuthRuntimeRequest(request, account)
	if coreErr != nil {
		return nil, coreErr
	}
	rejectedToken := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	attempt, attemptErr := managedOAuthAttemptForRequest(request, account)
	if attemptErr != nil {
		return nil, attemptErr
	}
	response, err := s.doOpenAIUpstreamOnce(request, proxyURL, account)
	attachManagedOAuthAttempt(response, request, attempt)
	if err == nil && response != nil && response.StatusCode == http.StatusUnauthorized && CoreOAuthDeclared(account) {
		s.recoverCoreOAuthRuntimeUnauthorized(request, account, coreBinding)
		return response, err
	}
	if err == nil && response != nil && response.StatusCode == http.StatusUnauthorized && ManagedOAuthDeclared(account) {
		if recordErr := recordManagedOAuthUnauthorized(request.Context(), s.accountRepo, attempt, rejectedToken); recordErr != nil {
			slog.Warn("managed_oauth.auth_event_persist_failed", "account_id", account.ID)
		}
		return response, nil
	}
	if err != nil || response == nil || response.StatusCode != http.StatusUnauthorized || openAIInferenceNoReplay(request.Context()) || s.openAITokenProvider == nil || request.Context().Err() != nil {
		return response, err
	}
	// Only replay bodies with an explicit rewind contract, before any response
	// body has been passed to the caller or an SSE frame has been committed.
	if request.Body != nil && request.Body != http.NoBody && request.GetBody == nil {
		return response, nil
	}
	credentialAccount, err := s.openAIOAuthCredentialOwner(request.Context(), account)
	if err != nil {
		return response, nil
	}
	if !canRecoverOpenAIOAuth(credentialAccount) || rejectedToken == "" || rejectedToken == request.Header.Get("Authorization") {
		return response, nil
	}
	retry := request.Clone(request.Context())
	if request.GetBody != nil {
		retry.Body, err = request.GetBody()
		if err != nil {
			return response, nil
		}
	}
	token, refreshErr := s.openAITokenProvider.RefreshAfterUnauthorized(request.Context(), credentialAccount, rejectedToken)
	if refreshErr != nil {
		if retry.Body != nil && retry.Body != http.NoBody {
			_ = retry.Body.Close()
		}
		// Preserve the original 401 and its normal account error policy. A
		// temporary refresh transport failure must not become a permanent ban.
		return response, nil
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	retry.Header.Set("Authorization", "Bearer "+token)
	response, err = s.doOpenAIUpstreamOnce(retry, proxyURL, account)
	if err == nil && response != nil && response.StatusCode == http.StatusUnauthorized && request.URL != nil && strings.HasSuffix(strings.TrimRight(request.URL.Path, "/"), "/responses") {
		// A tool-specific endpoint may deny access independently of inference.
		// Only the Responses endpoint establishes rejection of the repaired auth.
		s.openAITokenProvider.RecordUnauthorizedAfterRefresh(request.Context(), credentialAccount, token)
	}
	return response, err
}

func (s *OpenAIGatewayService) doOpenAIUpstreamOnce(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	var scopeErr error
	request, scopeErr = scopeManagedDeploymentHTTPRequest(request)
	if scopeErr != nil {
		return nil, scopeErr
	}
	request, scopeErr = s.PrepareManagedFileHTTPRequest(request, account)
	if scopeErr != nil {
		return nil, scopeErr
	}
	var attemptCtx context.Context
	var cleanup func() error
	request, attemptCtx, cleanup, scopeErr = s.PrepareStoredResponseRequest(request.Context(), account, request)
	if scopeErr != nil {
		return nil, scopeErr
	}
	if scopeErr = ClaimBPSResourceRequest(request, account); scopeErr != nil {
		if cleanup != nil {
			_ = cleanup()
		}
		return nil, scopeErr
	}
	if CoreOAuthDeclared(account) {
		request.GetBody = nil
	}
	snapshot := snapshotCodexTicketHTTPRequest(request, account)
	var response *http.Response
	var sendErr error
	handled := false
	if s.pluginManager != nil && !ManagedOAuthDeclared(account) && !CoreOAuthDeclared(account) && !IsBPSResourceRequest(request.Context()) {
		response, handled, sendErr = s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
	}
	if !handled {
		response, sendErr = s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
	}
	if sendErr != nil || response == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if cleanup != nil {
			_ = cleanup()
		}
		return response, sendErr
	}
	if prepareErr := s.PrepareStoredResponseHTTPResponse(attemptCtx, account, response); prepareErr != nil {
		if usage, reported := s.readUnacknowledgedStoredResponseUsage(attemptCtx, account, response); reported {
			prepareErr = &storedResponseUsageError{err: prepareErr, usage: usage}
		}
		prepareErr = withStoredResponseFailureUsage(attemptCtx, prepareErr)
		if response.Body != nil {
			_ = response.Body.Close()
		}
		if cleanup != nil {
			_ = cleanup()
		}
		return nil, prepareErr
	}
	if cleanup != nil {
		if response.Body == nil {
			_ = cleanup()
		} else {
			response.Body = &openAIResourceCleanupBody{ReadCloser: response.Body, cleanup: cleanup}
		}
	}
	s.observeCodexTicketHTTPResponse(snapshot, response)
	return response, nil
}

// doOpenAIAccountTestUpstream 让 OpenAI OAuth 账号测试与真实转发使用同一插件路径。
// API Key 和未命中插件的账号保持各自原有的 HTTPUpstream 行为。
func (s *AccountTestService) doOpenAIAccountTestUpstream(
	request *http.Request,
	proxyURL string,
	account *Account,
	useTLSFallback bool,
) (response *http.Response, returnErr error) {
	if CoreOAuthDeclared(account) {
		return nil, ErrCoreOAuthUnavailable
	}
	token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	attempt, attemptErr := managedOAuthAttemptForRequest(request, account)
	if attemptErr != nil {
		return nil, attemptErr
	}
	defer func() {
		attachManagedOAuthAttempt(response, request, attempt)
		if attempt != nil && returnErr == nil && response != nil && response.StatusCode == http.StatusUnauthorized {
			_ = recordManagedOAuthUnauthorized(request.Context(), s.accountRepo, attempt, token)
		}
	}()
	if s.pluginManager != nil && !ManagedOAuthDeclared(account) {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if useTLSFallback {
		return s.httpUpstream.DoWithTLS(
			request,
			proxyURL,
			account.ID,
			account.Concurrency,
			s.tlsFPProfileService.ResolveTLSProfile(account),
		)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}

type openAIResourceCleanupBody struct {
	io.ReadCloser
	cleanup  func() error
	once     sync.Once
	closeErr error
}

func (b *openAIResourceCleanupBody) Close() error {
	b.once.Do(func() {
		b.closeErr = b.ReadCloser.Close()
		if cleanupErr := b.cleanup(); b.closeErr == nil {
			b.closeErr = cleanupErr
		}
	})
	return b.closeErr
}

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
)

var errOpenAIRequestProxyUnavailable = errors.New("selected OpenAI request proxies are unavailable")

type openAIRequestProxyContextKey struct{}

const openAIRequestProxyOpsKey = "openai_request_proxy_ops_route"

type openAIRequestProxyRoute struct {
	accountID    int64
	proxy        string
	proxyID      int64
	proxyName    string
	releaseLease func()
	mu           sync.Mutex
	bodies       []*openAIRequestProxyBody
	cancelSends  []context.CancelFunc
	finished     bool
	once         sync.Once
}

func (r *openAIRequestProxyRoute) bind(req *http.Request) (*http.Request, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return nil, errOpenAIRequestProxyUnavailable
	}
	ctx, cancel := context.WithCancel(req.Context())
	r.cancelSends = append(r.cancelSends, cancel)
	return req.WithContext(ctx), nil
}

type openAIRequestProxyBody struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (b *openAIRequestProxyBody) Close() error {
	b.once.Do(func() { b.err = b.ReadCloser.Close() })
	return b.err
}
func (r *openAIRequestProxyRoute) track(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	b := &openAIRequestProxyBody{ReadCloser: resp.Body}
	resp.Body = b
	r.mu.Lock()
	r.bodies = append(r.bodies, b)
	closed := r.finished
	r.mu.Unlock()
	if closed {
		_ = b.Close()
	}
}
func (r *openAIRequestProxyRoute) finish() {
	r.once.Do(func() {
		r.mu.Lock()
		r.finished = true
		bodies := append([]*openAIRequestProxyBody(nil), r.bodies...)
		cancels := append([]context.CancelFunc(nil), r.cancelSends...)
		r.mu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		for _, b := range bodies {
			_ = b.Close()
		}
		if r.releaseLease != nil {
			r.releaseLease()
		}
	})
}

// Account tests return before their caller consumes the stream. Keep the
// request scope alive until that final body ends, closes, or is canceled.
type openAIRequestProxyOwnedBody struct {
	io.ReadCloser
	finish func()
}

func (b *openAIRequestProxyOwnedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.finish()
	}
	return n, err
}
func (b *openAIRequestProxyOwnedBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish()
	return err
}

// One logical HTTP turn owns its exit across payload repairs and continuations.
// The scope ends only after Forward finishes consuming its final response.
// SetOpenAIRequestProxyRepository supplies the same proxy catalog used by account administration.
func (s *OpenAIGatewayService) SetOpenAIRequestProxyRepository(repo ProxyRepository) {
	s.proxyRepo = repo
}

func (s *OpenAIGatewayService) requestProxyRepository() ProxyRepository {
	if s == nil {
		return nil
	}
	if s.proxyRepo != nil {
		return s.proxyRepo
	}
	if s.settingService != nil && s.settingService.proxyRepo != nil {
		return s.settingService.proxyRepo
	}
	if s.openAITokenProvider != nil && s.openAITokenProvider.openAIOAuthService != nil {
		return s.openAITokenProvider.openAIOAuthService.proxyRepo
	}
	return nil
}

func (s *OpenAIGatewayService) beginOpenAIRequestProxy(ctx context.Context, c *gin.Context, account *Account) (context.Context, func(), error) {
	return beginOpenAIRequestProxyWithRepository(ctx, c, account, s.requestProxyRepository())
}

func beginOpenAIRequestProxyWithRepository(ctx context.Context, c *gin.Context, account *Account, repo ProxyRepository) (context.Context, func(), error) {
	noop := func() {}
	// Every attempt replaces attribution, including a failed acquisition after
	// another account was tried on this Gin request.
	if c != nil {
		c.Set(openAIRequestProxyOpsKey, (*openAIRequestProxyRoute)(nil))
	}
	if account == nil || !account.OpenAIRequestProxyEnabled() {
		return ctx, noop, nil
	}
	if route, _ := ctx.Value(openAIRequestProxyContextKey{}).(*openAIRequestProxyRoute); route != nil {
		if route.accountID == account.ID {
			if _, err := ValidateOpenAIRequestProxyEgress(ctx, account.ID, route.proxy); err != nil {
				return ctx, noop, err
			}
			if c != nil {
				c.Set(openAIRequestProxyOpsKey, route)
			}
			return ctx, noop, nil
		}
		return ctx, noop, writeOpenAIRequestProxyError(c, "openai_proxy_unavailable", "Selected proxy scope does not match the account")
	}
	selected, releaseLease, err := acquireOpenAIRequestProxy(ctx, repo, account.OpenAIRequestProxyIDs())
	if err != nil {
		if ctx.Err() != nil {
			return ctx, noop, ctx.Err()
		}
		return ctx, noop, writeOpenAIRequestProxyError(c, "openai_proxy_unavailable", "No selected proxy is ready; direct fallback is disabled")
	}
	route := &openAIRequestProxyRoute{accountID: account.ID, proxy: selected.URL(), proxyID: selected.ID, proxyName: selected.Name, releaseLease: releaseLease}
	if c != nil {
		c.Set(openAIRequestProxyOpsKey, route)
	}
	release := route.finish
	stop := context.AfterFunc(ctx, release)
	scopedCtx := context.WithValue(ctx, openAIRequestProxyContextKey{}, route)
	if c != nil && c.Request != nil {
		c.Request = c.Request.WithContext(scopedCtx)
	}
	return scopedCtx, func() {
		stop()
		release()
		// Handler retries use c.Request.Context(). Remove only this route while
		// retaining request-local values installed by BPS or other adapters.
		if c != nil && c.Request != nil {
			if current, _ := c.Request.Context().Value(openAIRequestProxyContextKey{}).(*openAIRequestProxyRoute); current == route {
				c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), openAIRequestProxyContextKey{}, (*openAIRequestProxyRoute)(nil)))
			}
		}
	}, nil
}

func writeOpenAIRequestProxyError(c *gin.Context, code, message string) error {
	if c != nil && c.Writer != nil && !c.Writer.Written() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "upstream_error", "code": code, "message": message}})
	}
	return infraerrors.New(http.StatusServiceUnavailable, "OPENAI_PROXY_UNAVAILABLE", message).WithCause(errOpenAIRequestProxyUnavailable)
}

func openAIRequestProxyFromContext(ctx context.Context, accountID int64) (*openAIRequestProxyRoute, bool) {
	route, _ := ctx.Value(openAIRequestProxyContextKey{}).(*openAIRequestProxyRoute)
	return route, route != nil && route.accountID == accountID && route.proxy != ""
}

// ValidateOpenAIRequestProxyEgress lets transport adapters preserve a lease
// created by the gateway without exposing a way to create or replace it.
// The boolean distinguishes an ordinary request from an active scoped route.
func ValidateOpenAIRequestProxyEgress(ctx context.Context, accountID int64, proxyURL string) (bool, error) {
	if ctx == nil {
		return false, nil
	}
	route, _ := ctx.Value(openAIRequestProxyContextKey{}).(*openAIRequestProxyRoute)
	if route == nil {
		return false, nil
	}
	route.mu.Lock()
	defer route.mu.Unlock()
	if route.finished || route.accountID != accountID || route.proxy == "" || route.proxy != proxyURL {
		return true, errOpenAIRequestProxyUnavailable
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	return true, nil
}

// Counts are process-local and shared across accounts using the same exit.
// A fresh repository read prevents stale, deleted, expired, or disabled members
// from accepting a new request. Busy members remain eligible.
var openAIRequestProxyScheduler = struct {
	sync.Mutex
	inFlight map[int64]int
	next     uint64
}{inFlight: make(map[int64]int)}

func acquireOpenAIRequestProxy(ctx context.Context, repo ProxyRepository, ids []int64) (*Proxy, func(), error) {
	if repo == nil || len(ids) == 0 {
		return nil, nil, errOpenAIRequestProxyUnavailable
	}
	proxies, err := repo.ListByIDs(ctx, ids)
	if err != nil {
		return nil, nil, errOpenAIRequestProxyUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	allowed := make(map[int64]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}
	candidates := make(map[int64]Proxy, len(proxies))
	now := time.Now()
	for _, proxy := range proxies {
		if !allowed[proxy.ID] || !proxy.IsActive() || proxy.IsExpired(now) || strings.TrimSpace(proxy.Host) == "" || proxy.Port < 1 || proxy.Port > 65535 {
			continue
		}
		switch strings.ToLower(proxy.Protocol) {
		case "http", "https", "socks5", "socks5h":
		default:
			continue
		}
		parsed, err := url.Parse(proxy.URL())
		if err != nil || parsed.Hostname() == "" {
			continue
		}
		candidates[proxy.ID] = proxy
	}
	pool := make([]Proxy, 0, len(candidates))
	seen := map[int64]bool{}
	for _, id := range ids {
		if proxy, ok := candidates[id]; ok && !seen[id] {
			pool = append(pool, proxy)
			seen[id] = true
		}
	}
	if len(pool) == 0 {
		return nil, nil, errOpenAIRequestProxyUnavailable
	}
	openAIRequestProxyScheduler.Lock()
	start := int(openAIRequestProxyScheduler.next % uint64(len(pool)))
	selected := pool[start]
	for i := 1; i < len(pool); i++ {
		candidate := pool[(start+i)%len(pool)]
		if openAIRequestProxyScheduler.inFlight[candidate.ID] < openAIRequestProxyScheduler.inFlight[selected.ID] {
			selected = candidate
		}
	}
	openAIRequestProxyScheduler.next++
	openAIRequestProxyScheduler.inFlight[selected.ID]++
	openAIRequestProxyScheduler.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			openAIRequestProxyScheduler.Lock()
			defer openAIRequestProxyScheduler.Unlock()
			if openAIRequestProxyScheduler.inFlight[selected.ID] <= 1 {
				delete(openAIRequestProxyScheduler.inFlight, selected.ID)
			} else {
				openAIRequestProxyScheduler.inFlight[selected.ID]--
			}
		})
	}
	return &selected, release, nil
}

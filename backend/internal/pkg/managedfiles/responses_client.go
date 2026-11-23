package managedfiles

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	pkghttputil "github.com/MACOS-DO/sub4api/internal/pkg/httputil"
)

type ResponsesClientConfig struct {
	ControlURL, ServiceKey, ProxyURL string
	Expected                         Identity
	AccountID                        int64
	Concurrency                      int
}

type ResponsesHTTPUpstream interface {
	Do(*http.Request, string, int64, int) (*http.Response, error)
}

type ResponsesPreflightClient struct {
	config   ResponsesClientConfig
	upstream ResponsesHTTPUpstream
	baseURL  string
}

// The service must apply its outbound URL allowlist and no-redirect context
// before using this transport. No response or credential is included in errors.
func NewResponsesPreflightClient(config ResponsesClientConfig, upstream ResponsesHTTPUpstream) (*ResponsesPreflightClient, error) {
	u, err := url.Parse(config.ControlURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || strings.TrimSpace(config.ControlURL) != config.ControlURL ||
		(u.Path != "" && u.Path != "/" && path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/")) || upstream == nil || config.AccountID <= 0 || config.Concurrency <= 0 || !safeOpaque(config.ServiceKey, 4096) || ValidateIdentity(config.Expected) != nil {
		return nil, errors.New("responses preflight: invalid configuration")
	}
	return &ResponsesPreflightClient{config: config, upstream: upstream, baseURL: strings.TrimRight(config.ControlURL, "/")}, nil
}

func (c *ResponsesPreflightClient) Preflight(parent context.Context, owner, group string, client, wire []byte) (ResponsesPreflightResult, error) {
	var empty ResponsesPreflightResult
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	ctx, release := pkghttputil.WithRequestBodyMemoryScope(ctx)
	defer release()
	// Encoding retains the exact two bodies and may grow its backing buffer.
	// Reserve before both strict validation and envelope materialization.
	if err := pkghttputil.ReserveRequestBodyMemory(ctx, 3*(int64(len(client))+int64(len(wire))+(16<<10))); err != nil {
		return empty, err
	}
	request := ResponsesPreflightRequest{Version: ResponsesContract, Expected: c.config.Expected, OwnerRef: owner, GroupRef: group, ClientRequest: client, WireRequest: wire}
	encoded, err := EncodeResponsesPreflight(request)
	if err != nil {
		return empty, errors.New("responses preflight: invalid request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+ResponsesPrefix+"preflight", bytes.NewReader(encoded))
	if err != nil {
		return empty, errors.New("responses preflight: invalid request")
	}
	req.GetBody = nil
	req.Header.Set(ServiceKeyHeader, c.config.ServiceKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.upstream.Do(req, c.config.ProxyURL, c.config.AccountID, c.config.Concurrency)
	if err != nil {
		return empty, errors.New("responses preflight: transport failed")
	}
	if resp == nil || resp.Body == nil {
		return empty, errors.New("responses preflight: invalid response")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 {
		return empty, errors.New("responses preflight: invalid response")
	}
	if resp.StatusCode != http.StatusOK {
		values := resp.Header.Values("X-Bps-Request-Policy")
		if len(values) == 1 && values[0] == "rejected-v1" {
			if public := decodeResponsesPolicyError(resp.StatusCode, raw); public != nil {
				return empty, public
			}
		}
		return empty, errors.New("responses preflight: rejected")
	}
	result, err := DecodeResponsesPreflightResult(raw)
	if err != nil {
		return empty, errors.New("responses preflight: invalid response")
	}
	hash := ResponsesWireSHA256(wire)
	binding, err := ResponsesBindingSHA256(request, result.PolicyVersion, hash)
	now := time.Now().Unix()
	if err != nil || result.Identity != c.config.Expected || result.WireSHA256 != hash || result.BindingSHA256 != binding || result.ExpiresAt <= now || result.ExpiresAt > now+31 {
		return empty, errors.New("responses preflight: binding mismatch")
	}
	return result, nil
}

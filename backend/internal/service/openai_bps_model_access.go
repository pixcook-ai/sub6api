package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var bpsModelIDPattern = regexp.MustCompile(`^gpt-[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// bpsAccessModelIDs converts the official BPS access response into model IDs.
// An explicit denial is authoritative (empty list); a malformed envelope is an
// error and must never be turned into a local allowlist.
func bpsAccessModelIDs(raw []byte) ([]string, error) {
	var envelope struct {
		Allowed *bool `json:"allowed"`
		Catalog *struct {
			Models     *[]json.RawMessage `json:"models"`
			Restricted []string           `json:"restricted_models"`
		} `json:"model_catalog"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Allowed == nil {
		return nil, fmt.Errorf("invalid BPS access response")
	}
	ids := []string{}
	if !*envelope.Allowed {
		return ids, nil
	}
	if envelope.Catalog == nil || envelope.Catalog.Models == nil {
		return nil, fmt.Errorf("missing BPS model catalog")
	}
	restricted := map[string]bool{}
	for _, id := range envelope.Catalog.Restricted {
		restricted[id] = true
	}
	seen := map[string]bool{}
	for _, rawEntry := range *envelope.Catalog.Models {
		var entry struct {
			ID        string `json:"id"`
			Available *bool  `json:"available"`
			Enabled   *bool  `json:"enabled"`
			Picker    *bool  `json:"model_picker_enabled"`
			Policy    *struct {
				State string `json:"state"`
			} `json:"policy"`
		}
		if json.Unmarshal(rawEntry, &entry) != nil || !bpsModelIDPattern.MatchString(entry.ID) || restricted[entry.ID] || seen[entry.ID] {
			continue
		}
		if (entry.Available != nil && !*entry.Available) || (entry.Enabled != nil && !*entry.Enabled) || (entry.Picker != nil && !*entry.Picker) {
			continue
		}
		if entry.Policy != nil && strings.EqualFold(entry.Policy.State, "disabled") {
			continue
		}
		seen[entry.ID] = true
		ids = append(ids, entry.ID)
	}
	return ids, nil
}

// FetchOpenAIBPSModelIDs asks BPS which models this account may use. Results are
// cached per credential, workspace and proxy; only fresh entries are served, so
// revoked permissions are never answered from stale data.
func (s *OpenAIGatewayService) FetchOpenAIBPSModelIDs(ctx context.Context, account *Account) ([]string, error) {
	if s == nil || s.httpUpstream == nil || account == nil || !account.IsOpenAIBPS() {
		return nil, fmt.Errorf("BPS model discovery is unavailable")
	}
	ctx, cancel := context.WithTimeout(WithHTTPUpstreamRedirectsDisabled(ctx), 5*time.Second)
	defer cancel()
	req, err := buildOpenAIBPSRequest(ctx, account, nil)
	if err != nil {
		return nil, err
	}
	req.Method = http.MethodGet
	req.Body = nil
	req.GetBody = nil
	req.ContentLength = 0
	req.Header.Del("Content-Type")
	req.Header.Set("Accept", "application/json")
	req.URL.Path = "/basispoints/api/responses/access"
	req.URL.RawQuery = "include_models=true"
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	key := "bps_models:" + bpsDigest(token+"\x00"+req.Header.Get("chatgpt-account-id")+"\x00"+proxyURL)
	if cached, state := s.openAIModelsCache.get(key, time.Now()); state == openAIModelsCacheFresh && cached != nil {
		var ids []string
		if json.Unmarshal(cached.Body, &ids) == nil {
			return ids, nil
		}
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("BPS model discovery request failed")
	}
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("BPS model discovery returned no response")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("BPS model discovery returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, openAIModelsCacheBodyLimit+1))
	if err != nil || len(raw) > openAIModelsCacheBodyLimit {
		return nil, fmt.Errorf("BPS model catalog could not be read")
	}
	ids, err := bpsAccessModelIDs(raw)
	if err != nil {
		return nil, err
	}
	if body, marshalErr := json.Marshal(ids); marshalErr == nil {
		s.openAIModelsCache.set(key, &OpenAIModelsResponse{Body: body}, time.Now())
	}
	return ids, nil
}

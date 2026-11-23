package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/stretchr/testify/require"
)

const bpsSessionDescribeTestKey = "session-binding-test-key-0000000000000000"
const bpsSessionDescribeTestFeature = "X-Bps-Session-Binding-Feature"

func bpsSessionDescribeTestBackend(t *testing.T, server *httptest.Server) (*BPSFilesBackend, *bpsFilesTestTransport) {
	t.Helper()
	transport := &bpsFilesTestTransport{client: server.Client()}
	backend := bpsFilesTestBackend(t, server.URL, transport)
	backend.config.ServiceKey = bpsSessionDescribeTestKey
	return backend, transport
}

func TestBPSFilesBackendDescribeSessionBindingAuthenticatesExactFeature(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/internal/bps-files/v1/describe", r.URL.Path)
		require.Equal(t, []string{"bps-session-binding/1"}, r.Header.Values(bpsSessionDescribeTestFeature))
		require.Equal(t, bpsSessionDescribeTestKey, r.Header.Get(managedfiles.ServiceKeyHeader))
		require.Empty(t, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("X-Bps-Session-Binding"))
		w.Header().Set(bpsSessionDescribeTestFeature, "bps-session-binding/1")
		require.NoError(t, json.NewEncoder(w).Encode(bpsFilesTestDescription()))
	}))
	defer server.Close()
	backend, transport := bpsSessionDescribeTestBackend(t, server)
	got, err := backend.DescribeSessionBinding(context.Background())
	require.NoError(t, err)
	require.Equal(t, bpsFilesTestDescription(), got)
	require.Equal(t, 1, transport.calls)
}

func TestBPSFilesBackendDescribeSessionBindingRejectsMissingOrAmbiguousFeature(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
	}{
		{name: "old gateway"},
		{name: "unknown version", values: []string{"bps-session-binding/2"}},
		{name: "duplicate", values: []string{"bps-session-binding/1", "bps-session-binding/1"}},
		{name: "list value", values: []string{"bps-session-binding/1,bps-session-binding/1"}},
		{name: "empty", values: []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for _, value := range tc.values {
					w.Header().Add(bpsSessionDescribeTestFeature, value)
				}
				require.NoError(t, json.NewEncoder(w).Encode(bpsFilesTestDescription()))
			}))
			defer server.Close()
			backend, transport := bpsSessionDescribeTestBackend(t, server)
			got, err := backend.DescribeSessionBinding(context.Background())
			require.Error(t, err)
			require.Zero(t, got)
			require.Equal(t, 1, transport.calls)
			require.NotContains(t, err.Error(), bpsSessionDescribeTestKey)
		})
	}
}

func TestBPSFilesBackendDescribeSessionBindingPreservesDescriptionValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*managedfiles.DescribeResult)
	}{
		{name: "identity rotation", change: func(d *managedfiles.DescribeResult) { d.CredentialGeneration = "rotated" }},
		{name: "missing capability", change: func(d *managedfiles.DescribeResult) { d.Capabilities.EnsureUpload = false }},
		{name: "invalid limit", change: func(d *managedfiles.DescribeResult) { d.Limits.FileBytes = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(bpsSessionDescribeTestFeature, "bps-session-binding/1")
				description := bpsFilesTestDescription()
				tc.change(&description)
				require.NoError(t, json.NewEncoder(w).Encode(description))
			}))
			defer server.Close()
			backend, transport := bpsSessionDescribeTestBackend(t, server)
			_, err := backend.DescribeSessionBinding(context.Background())
			require.Error(t, err)
			require.Equal(t, 1, transport.calls)
		})
	}
}

func TestBPSFilesBackendDescribeSessionBindingDoesNotCacheIdentity(t *testing.T) {
	var observed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := observed.Add(1)
		w.Header().Set(bpsSessionDescribeTestFeature, "bps-session-binding/1")
		description := bpsFilesTestDescription()
		if attempt > 1 {
			description.CredentialGeneration = "rotated"
		}
		require.NoError(t, json.NewEncoder(w).Encode(description))
	}))
	defer server.Close()
	backend, transport := bpsSessionDescribeTestBackend(t, server)
	_, err := backend.DescribeSessionBinding(context.Background())
	require.NoError(t, err)
	_, err = backend.DescribeSessionBinding(context.Background())
	require.Error(t, err)
	require.Equal(t, 2, transport.calls)
}

func TestBPSFilesBackendDescribeSessionBindingStopsBeforeSendForInvalidKeyOrCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid or canceled negotiation reached the backend")
	}))
	defer server.Close()
	backend, transport := bpsSessionDescribeTestBackend(t, server)
	backend.config.ServiceKey = strings.Repeat("s", 31)
	_, err := backend.DescribeSessionBinding(context.Background())
	require.Error(t, err)
	require.Zero(t, transport.calls)
	backend.config.ServiceKey = bpsSessionDescribeTestKey
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = backend.DescribeSessionBinding(ctx)
	require.Error(t, err)
	require.Zero(t, transport.calls)
}

func TestBPSFilesBackendDescribeSessionBindingFailureDoesNotLeakOrRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(bpsSessionDescribeTestFeature, "bps-session-binding/1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "private-upstream-payload-marker "+bpsSessionDescribeTestKey)
	}))
	defer server.Close()
	backend, transport := bpsSessionDescribeTestBackend(t, server)
	_, err := backend.DescribeSessionBinding(context.Background())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private-upstream-payload-marker")
	require.NotContains(t, err.Error(), bpsSessionDescribeTestKey)
	require.NotContains(t, err.Error(), server.URL)
	require.Equal(t, 1, transport.calls)
}

type bpsSessionDescribeCloseBody struct {
	io.Reader
	closed bool
}

func (b *bpsSessionDescribeCloseBody) Close() error { b.closed = true; return nil }

type bpsSessionDescribeResponseUpstream struct {
	HTTPUpstream
	response *http.Response
}

func (u *bpsSessionDescribeResponseUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return u.response, nil
}

func TestBPSFilesBackendDescribeSessionBindingRejectsAmbiguousFeatureAndClosesBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
	}{
		{name: "leading whitespace", header: http.Header{bpsSessionDescribeTestFeature: []string{" bps-session-binding/1"}}},
		{name: "trailing whitespace", header: http.Header{bpsSessionDescribeTestFeature: []string{"bps-session-binding/1 "}}},
		{name: "case alias duplicate", header: http.Header{
			bpsSessionDescribeTestFeature:   []string{"bps-session-binding/1"},
			"x-bps-session-binding-feature": []string{"bps-session-binding/1"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(bpsFilesTestDescription())
			require.NoError(t, err)
			body := &bpsSessionDescribeCloseBody{Reader: strings.NewReader(string(encoded))}
			upstream := &bpsSessionDescribeResponseUpstream{response: &http.Response{StatusCode: http.StatusOK, Header: tc.header, Body: body}}
			backend := &BPSFilesBackend{config: BPSFilesBackendConfig{ServiceKey: bpsSessionDescribeTestKey, Expected: bpsFilesTestIdentity(), AccountID: 7, Concurrency: 1}, upstream: upstream, baseURL: "https://gateway.example"}
			_, err = backend.DescribeSessionBinding(context.Background())
			require.Error(t, err)
			require.True(t, body.closed)
		})
	}
}

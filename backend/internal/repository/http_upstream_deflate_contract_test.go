package repository

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// HTTP deflate is a zlib-wrapped stream. Keep accepting common raw DEFLATE from
// older upstreams, but exercise both through the public HTTPUpstream.Do path.
func TestHTTPUpstreamDeflateSSEContract(t *testing.T) {
	const payload = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, encoding := range []string{"standard_zlib", "legacy_raw"} {
		t.Run(encoding, func(t *testing.T) {
			var encoded []byte
			if encoding == "standard_zlib" {
				var buffer bytes.Buffer
				writer := zlib.NewWriter(&buffer)
				_, err := io.WriteString(writer, payload)
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				encoded = buffer.Bytes()
			} else {
				encoded = compressDeflate(t, []byte(payload))
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.Header.Get("Accept-Encoding"), "deflate") {
					t.Error("request did not advertise deflate")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", "deflate")
				w.Header().Set("Content-Length", fmt.Sprint(len(encoded)))
				_, _ = w.Write(encoded)
			}))
			defer server.Close()
			upstream := NewHTTPUpstream(nil).(*httpUpstreamService)
			t.Cleanup(func() {
				upstream.mu.RLock()
				defer upstream.mu.RUnlock()
				for _, entry := range upstream.clients {
					entry.client.CloseIdleConnections()
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
			require.NoError(t, err)
			request.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
			response, err := upstream.Do(request, "", 1, 1)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err, "HTTP deflate must decode before SSE parsing")
			require.Equal(t, payload, string(body))
			require.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
			require.Empty(t, response.Header.Get("Content-Encoding"))
			require.Empty(t, response.Header.Get("Content-Length"))
			require.Equal(t, int64(-1), response.ContentLength)
			require.NoError(t, response.Body.Close())
			requireNoUpstreamInFlight(t, upstream)
		})
	}
}

func TestHTTPUpstreamDeflateReturnsBeforeFirstFrame(t *testing.T) {
	for _, mode := range []string{"read_after_headers", "close_before_body", "close_pending_read"} {
		t.Run(mode, func(t *testing.T) {
			var encoded bytes.Buffer
			writer := zlib.NewWriter(&encoded)
			_, err := io.WriteString(writer, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			allowBody := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Encoding", "deflate")
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_ = http.NewResponseController(w).Flush()
				select {
				case <-allowBody:
					_, _ = w.Write(encoded.Bytes())
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			defer close(allowBody)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
			require.NoError(t, err)
			request.Header.Set("Accept-Encoding", "deflate")
			upstream := NewHTTPUpstream(nil).(*httpUpstreamService)
			result := make(chan *http.Response, 1)
			go func() {
				response, requestErr := upstream.Do(request, "", 1, 1)
				if requestErr != nil {
					t.Errorf("Do failed: %v", requestErr)
				}
				result <- response
			}()
			var response *http.Response
			select {
			case response = <-result:
				require.NotNil(t, response)
			case <-time.After(time.Second):
				cancel()
				if late := <-result; late != nil {
					_ = late.Body.Close()
				}
				t.Fatal("Do waited for a deflate body before returning flushed headers")
			}
			defer func() { _ = response.Body.Close() }()
			if mode == "read_after_headers" {
				// Only send the first frame once the caller owns the response.
				allowBody <- struct{}{}
				body, readErr := io.ReadAll(response.Body)
				require.NoError(t, readErr)
				require.Equal(t, "event: ping\ndata: {\"type\":\"ping\"}\n\n", string(body))
			} else {
				var readDone chan error
				if mode == "close_pending_read" {
					readDone = make(chan error, 1)
					started := make(chan struct{})
					response.Body = &notifyReadCloser{ReadCloser: response.Body, started: started}
					go func() {
						_, readErr := io.ReadAll(response.Body)
						readDone <- readErr
					}()
					<-started
				}
				closed := make(chan error, 1)
				go func() { closed <- response.Body.Close() }()
				select {
				case closeErr := <-closed:
					require.NoError(t, closeErr)
				case <-time.After(time.Second):
					t.Fatal("Close waited for the first deflate frame")
				}
				if readDone != nil {
					select {
					case readErr := <-readDone:
						require.Error(t, readErr)
					case <-time.After(time.Second):
						t.Fatal("Close did not unblock the pending deflate read")
					}
				}
			}
			require.NoError(t, response.Body.Close())
			requireNoUpstreamInFlight(t, upstream)
		})
	}
}

func TestDecompressDeflateRejectsDictionary(t *testing.T) {
	var encoded bytes.Buffer
	writer, err := zlib.NewWriterLevelDict(&encoded, zlib.DefaultCompression, []byte("synthetic dictionary"))
	require.NoError(t, err)
	_, err = io.WriteString(writer, "synthetic dictionary response")
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	response := newEncodedResponse("deflate", encoded.Bytes())
	decompressResponseBody(response)
	_, err = io.ReadAll(response.Body)
	require.ErrorIs(t, err, zlib.ErrDictionary, "an unavailable dictionary must not be accepted as raw DEFLATE")
	require.NoError(t, response.Body.Close())
}

func TestDecompressDeflateCorruptChecksum(t *testing.T) {
	var encoded bytes.Buffer
	writer := zlib.NewWriter(&encoded)
	_, err := io.WriteString(writer, "synthetic response")
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	payload := encoded.Bytes()
	payload[len(payload)-1] ^= 1
	response := newEncodedResponse("deflate", payload)
	decompressResponseBody(response)
	_, err = io.ReadAll(response.Body)
	require.ErrorIs(t, err, zlib.ErrChecksum, "a corrupt zlib stream must not be retried as raw DEFLATE")
	require.NoError(t, response.Body.Close())
}

package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

func TestFilesUploadMissingFreshAuthorizationFailsClosed(t *testing.T) {
	cfg := config.DefaultFilesConfig()
	cfg.Enabled, cfg.Root = true, t.TempDir()
	files, err := service.NewFileService(&struct{ service.FileRepository }{}, &struct{ service.FileBlobStore }{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := NewFileHandler(files)
	r := gin.New()
	r.POST("/v1/files", func(c *gin.Context) {
		group := int64(42)
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 11, UserID: 7, GroupID: &group, Group: &service.Group{Platform: service.PlatformOpenAI}})
		h.Upload(c)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/files", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing fresh authorization returned %d: %s", w.Code, w.Body.String())
	}
}

func TestFileUploadDeadlineRejectsUnsupportedTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stop, err := bindFileUploadDeadline(httptest.NewRecorder(), ctx)
	if stop != nil || !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("unsupported writer accepted: %v", err)
	}
}

func TestFileUploadDeadlineInterruptsRealGinBodyRead(t *testing.T) {
	for _, earlyCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "lease cancellation"}[earlyCancel], func(t *testing.T) {
			observed := make(chan error, 1)
			r := gin.New()
			r.POST("/", func(c *gin.Context) {
				ctx, cancel := context.WithTimeout(c.Request.Context(), 100*time.Millisecond)
				defer cancel()
				stop, err := bindFileUploadDeadline(c.Writer, ctx)
				if err != nil {
					observed <- err
					return
				}
				defer stop()
				if earlyCancel {
					cancel()
				}
				_, err = io.ReadAll(c.Request.Body)
				if err == nil {
					err = fmt.Errorf("incomplete upload read unexpectedly succeeded")
				}
				observed <- err
			})
			server := httptest.NewServer(r)
			defer server.Close()
			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err = fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: 20\r\n\r\nx"); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-observed:
				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatalf("expected real socket read timeout, got %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("upload remained blocked past cancellation/deadline")
			}
		})
	}
}

package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func TestStoredNativeToolContextUsesResolvedLocalParent(t *testing.T) {
	body := []byte(`{"model":"public","store":true,"input":[{"type":"function_call_output","call_id":"call_read","output":"done"}]}`)
	parent := "resp_local_" + strings.Repeat("a", 32)
	plan, err := service.NewStoredResponseNativePlan15(service.StoredResponseOwner{UserID: 1, APIKeyID: 2, GroupID: 3}, body, parent, []byte(`{"version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		plan *service.StoredResponsePlan
		body []byte
		want bool
	}{
		{"resolved local history", plan, body, true},
		{"no local history", nil, body, false},
		{"missing call id", plan, []byte(`{"model":"public","input":[{"type":"function_call_output","output":"done"}]}`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			if tc.plan != nil {
				c.Request = c.Request.WithContext(service.WithStoredResponsePlan(c.Request.Context(), tc.plan))
			}
			if got := (&OpenAIGatewayHandler{}).validateFunctionCallOutputRequest(c, tc.body, zap.NewNop()); got != tc.want {
				t.Fatalf("allowed=%v, want %v", got, tc.want)
			}
		})
	}
}

package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountCreateCapabilities(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &AccountHandler{}
	router := gin.New()
	router.GET("/accounts/:id", handler.GetByID)
	router.GET("/accounts/create-capabilities", handler.CreateCapabilities)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/accounts/create-capabilities", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	var body struct {
		Code int `json:"code"`
		Data struct {
			SchedulableOnCreate bool `json:"schedulable_on_create"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Zero(t, body.Code)
	require.True(t, body.Data.SchedulableOnCreate)
}

func TestAccountCreateForwardsInitialSchedulable(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, field := range []string{"", `,"schedulable":null`, `,"schedulable":false`, `,"schedulable":true`} {
			t.Run(field+map[bool]string{true: "/batch", false: "/single"}[batch], func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				stub := newStubAdminService()
				handler := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				router := gin.New()
				body := `{"name":"account","platform":"anthropic","type":"apikey","credentials":{"api_key":"synthetic"}` + field + `}`
				if batch {
					router.POST("/accounts", handler.BatchCreate)
					body = `{"accounts":[` + body + `]}`
				} else {
					router.POST("/accounts", handler.Create)
				}
				request := httptest.NewRequest(http.MethodPost, "/accounts", bytes.NewBufferString(body))
				request.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.Len(t, stub.createdAccounts, 1)
				got := stub.createdAccounts[0].Schedulable
				switch field {
				case "", `,"schedulable":null`:
					require.Nil(t, got)
				case `,"schedulable":false`:
					require.NotNil(t, got)
					require.False(t, *got)
				case `,"schedulable":true`:
					require.NotNil(t, got)
					require.True(t, *got)
				}
			})
		}
	}
}

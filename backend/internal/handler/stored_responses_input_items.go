package handler

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

type storedInputItemsQuery struct {
	after string
	limit int
	order string
}

func parseStoredInputItemsQuery(raw string) (storedInputItemsQuery, string) {
	query := storedInputItemsQuery{limit: 20, order: "desc"}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return query, "query"
	}
	for name, list := range values {
		if len(list) != 1 {
			return query, name
		}
		value := list[0]
		switch name {
		case "after":
			if value == "" || len(value) > 256 {
				return query, name
			}
			query.after = value
		case "limit":
			limit, err := strconv.Atoi(value)
			if err != nil || limit < 1 || limit > 100 {
				return query, name
			}
			query.limit = limit
		case "order":
			if value != "asc" && value != "desc" {
				return query, name
			}
			query.order = value
		case "include", "include[]":
			// Stored text inputs have no optional hosted-tool or private fields.
			// Never satisfy encrypted-content includes using private recovery.
			if value != "" {
				return query, "include"
			}
		default:
			return query, name
		}
	}
	return query, ""
}

func storedInputItemsQueryError(c *gin.Context, param string) {
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{
		"type": "invalid_request_error", "code": "invalid_parameter", "param": param,
		"message": "Invalid stored response query parameter.",
	}})
}

func parseStoredResponseRetrieveQuery(raw string) string {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "query"
	}
	for name, list := range values {
		if len(list) != 1 {
			return name
		}
		switch name {
		case "stream":
			// Replaying original SSE events requires a separate persistent event
			// contract. Only an explicit non-streaming retrieve is supported.
			if list[0] != "false" {
				return name
			}
		case "include", "include[]":
			if list[0] != "" {
				return "include"
			}
		default:
			return name
		}
	}
	return ""
}

func (h *StoredResponseHandler) InputItems(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	query, invalid := parseStoredInputItemsQuery(c.Request.URL.RawQuery)
	if invalid != "" {
		storedInputItemsQueryError(c, invalid)
		return
	}
	owner, ok := storedResponseOwner(c)
	if !ok {
		return
	}
	record, err := h.responses.Get(c.Request.Context(), owner, c.Param("response_id"))
	if err != nil {
		handleStoredResponseError(c, err)
		return
	}
	if record == nil || !storedResponseModelAllowed(c, record.Model) {
		if record == nil {
			storedResponseUnavailable(c)
		}
		return
	}
	items, err := service.StoredResponseInputItems(record)
	if err != nil {
		storedResponseUnavailable(c)
		return
	}
	if query.order == "desc" {
		for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
			items[left], items[right] = items[right], items[left]
		}
	}
	start := 0
	if query.after != "" {
		found := false
		for index, item := range items {
			if item["id"] == query.after {
				start, found = index+1, true
				break
			}
		}
		if !found {
			storedInputItemsQueryError(c, "after")
			return
		}
	}
	end := start + query.limit
	if end > len(items) {
		end = len(items)
	}
	data := items[start:end]
	var firstID, lastID any
	if len(data) != 0 {
		firstID, lastID = data[0]["id"], data[len(data)-1]["id"]
	}
	// These deterministic cursors are local projections of an authorized row,
	// not newly observed upstream resources. Keep other resource checks active.
	if writer, ok := c.Writer.(*managedDeploymentResponseWriter); ok {
		writer.ctx = service.WithStoredResponseInputItemsProjection(c.Request.Context(), record)
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": data, "has_more": end < len(items), "first_id": firstID, "last_id": lastID})
}

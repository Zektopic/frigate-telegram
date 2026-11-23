package restapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAPIKeyMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		apiKey     string
		header     string
		wantStatus int
	}{
		{"no key configured", "", "", http.StatusOK},
		{"missing key", "s3cret", "", http.StatusUnauthorized},
		{"wrong key", "s3cret", "s3cres", http.StatusUnauthorized},
		{"key prefix", "s3cret", "s3c", http.StatusUnauthorized},
		{"correct key", "s3cret", "s3cret", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := gin.New()
			r.Use(apiKeyMiddleware(tt.apiKey))
			r.GET("/status", func(c *gin.Context) { c.Status(http.StatusOK) })

			req := httptest.NewRequest(http.MethodGet, "/status", nil)
			if tt.header != "" {
				req.Header.Set("X-API-Key", tt.header)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tt.wantStatus {
				t.Errorf("got status %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}

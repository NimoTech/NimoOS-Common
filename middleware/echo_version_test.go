package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestRegisterVersionRoute(t *testing.T) {
	e := echo.New()
	RegisterVersionRoute(e, "/v1/foo/version", "Foo", "1.2.3")

	req := httptest.NewRequest(http.MethodGet, "/v1/foo/version", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got VersionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != "Foo" || got.Version != "1.2.3" {
		t.Fatalf("got %+v, want {Foo 1.2.3}", got)
	}
}

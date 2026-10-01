package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeChecker map[string]error

func (f fakeChecker) Check(context.Context) map[string]error { return f }

func get(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body không phải JSON: %v", err)
	}
	return rec.Code, body
}

func TestLiveDoesNotTouchDependencies(t *testing.T) {
	h := NewRouter(fakeChecker{"primary": errors.New("down")})
	code, body := get(t, h, "/health/live")
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("live = %d %v", code, body)
	}
}

func TestReadyOK(t *testing.T) {
	h := NewRouter(fakeChecker{"primary": nil, "replica": nil})
	code, body := get(t, h, "/health/ready")
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("ready = %d %v", code, body)
	}
}

func TestReadyReportsFailingDependency(t *testing.T) {
	h := NewRouter(fakeChecker{"primary": nil, "replica": errors.New("connection refused")})
	code, body := get(t, h, "/health/ready")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("ready code = %d", code)
	}
	checks := body["checks"].(map[string]any)
	if checks["primary"] != "ok" || checks["replica"] != "unavailable" {
		t.Fatalf("checks = %v", checks)
	}
}

func TestReadyDoesNotLeakErrorDetails(t *testing.T) {
	secret := "dial tcp 10.0.0.5:5432: user=admin database=prod"
	h := NewRouter(fakeChecker{"primary": errors.New(secret)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if strings.Contains(rec.Body.String(), "10.0.0.5") || strings.Contains(rec.Body.String(), "admin") {
		t.Fatalf("body lộ chi tiết lỗi: %s", rec.Body.String())
	}
}

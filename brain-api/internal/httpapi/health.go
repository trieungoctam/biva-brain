// Package httpapi chứa các endpoint HTTP của brain-api.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Checker kiểm tra phụ thuộc (DB...). Trả về lỗi theo tên phụ thuộc; nil = ổn.
type Checker interface {
	Check(ctx context.Context) map[string]error
}

// NewRouter dựng router với các endpoint health; MCP được gắn thêm vào mux trả về.
//   - /health/live  : process còn sống, không chạm DB.
//   - /health/ready : mọi phụ thuộc trả lời được trong 2 giây. Chi tiết lỗi chỉ ghi log,
//     không trả ra ngoài (tránh lộ host/user/database).
func NewRouter(checker Checker) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		checks := map[string]string{}
		status := http.StatusOK
		for name, err := range checker.Check(ctx) {
			if err != nil {
				slog.Warn("health check thất bại", "dependency", name, "err", err)
				checks[name] = "unavailable"
				status = http.StatusServiceUnavailable
				continue
			}
			checks[name] = "ok"
		}
		state := "ok"
		if status != http.StatusOK {
			state = "unavailable"
		}
		writeJSON(w, status, map[string]any{"status": state, "checks": checks})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

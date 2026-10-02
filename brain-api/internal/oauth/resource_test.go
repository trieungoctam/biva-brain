package oauth

import "testing"

// Resource rỗng /mcp/operator/ phải bị từ chối (từng match prefix mọi nhà xe); đúng id thì qua.
func TestValidResource(t *testing.T) {
	s := &Server{Issuer: "https://brain.biva.vn"}
	bad := []string{
		"https://evil.example/mcp/operator/x/",
		"https://brain.biva.vn/mcp/operator/", // id rỗng
		"https://brain.biva.vn/mcp/operator",
		"https://brain.biva.vn/mcp/other/",
		"https://brain.biva.vn/mcp/operator/../platform/",
	}
	for _, r := range bad {
		if s.validResource(r) {
			t.Fatalf("phải từ chối %q", r)
		}
	}
	for _, r := range []string{
		"https://brain.biva.vn",
		"https://brain.biva.vn/mcp/platform/",
		"https://brain.biva.vn/mcp/operator/phuongnam/",
	} {
		if !s.validResource(r) {
			t.Fatalf("phải chấp nhận %q", r)
		}
	}
}

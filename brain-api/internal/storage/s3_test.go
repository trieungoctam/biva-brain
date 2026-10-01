package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestS3Roundtrip chạy khi có BIVA_TEST_S3_URL (SeaweedFS/MinIO thật, vd http://localhost:8333):
// EnsureBucket → Upload → GET lại đúng nội dung qua URL công khai.
func TestS3Roundtrip(t *testing.T) {
	endpoint := os.Getenv("BIVA_TEST_S3_URL")
	if endpoint == "" {
		t.Skip("đặt BIVA_TEST_S3_URL để chạy test với S3 thật")
	}
	s, err := New(endpoint, endpoint, "biva-test", os.Getenv("BIVA_TEST_S3_ACCESS_KEY"), os.Getenv("BIVA_TEST_S3_SECRET_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}

	key := fmt.Sprintf("test/%d/xu-at.json", time.Now().UnixNano())
	want := "{\"operator\":\"pilot1\"}"
	got, err := s.Upload(ctx, key, ContentType("json"), []byte(want))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, strings.TrimRight(endpoint, "/")+"/biva-test/"+key) {
		t.Fatalf("URL = %s", got)
	}
	u, _ := url.Parse(got)
	var body string
	ok := pollUntil(10*time.Second, func() bool {
		resp, err := http.Get(u.String())
		if err != nil {
			t.Logf("GET: %v", err)
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Logf("GET status %d", resp.StatusCode)
			return false
		}
		b, _ := io.ReadAll(resp.Body)
		body = string(b)
		return true
	})
	if !ok {
		t.Fatalf("không GET lại được %s", got)
	}
	if body != want {
		t.Fatalf("nội dung = %q, muốn %q", body, want)
	}
}

func pollUntil(deadline time.Duration, fn func() bool) bool {
	for start := time.Now(); time.Since(start) < deadline; time.Sleep(500 * time.Millisecond) {
		if fn() {
			return true
		}
	}
	return fn()
}

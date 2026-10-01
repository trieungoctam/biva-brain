// rerank.go — S3.1.3: rerank TEI CPU (bge-reranker-v2-m3) cho recall.
//
// Chạy sau RRF trên top ≤ 50 ứng viên; ngân sách 80 ms (rerankBudget) — quá hạn hoặc lỗi thì giữ
// nguyên thứ tự RRF và ghi "rerank" vào Degraded: TEI chết → recall vẫn trả kết quả.
package recall

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

const (
	rerankBudget = 80 * time.Millisecond
	rerankTop    = 50 // top 30–50 ứng viên theo docs/architecture.md
)

// Reranker xếp lại thứ tự tài liệu theo mức liên quan tới query; trả index theo thứ tự mới.
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string) ([]int, error)
}

// TEIRerank gọi POST /rerank của text-embeddings-inference (bge-reranker-v2-m3).
type TEIRerank struct {
	URL    string
	Client *http.Client
}

func NewTEIRerank(url string) *TEIRerank {
	return &TEIRerank{URL: url, Client: &http.Client{Timeout: 2 * time.Second}}
}

func (t *TEIRerank) Rerank(ctx context.Context, query string, docs []string) ([]int, error) {
	body, err := json.Marshal(map[string]any{"query": query, "texts": docs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("TEI rerank %d: %s", resp.StatusCode, string(b))
	}
	var out []struct {
		Index int     `json:"index"`
		Score float64 `json:"score"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	order := make([]int, 0, len(out))
	for _, o := range out {
		if o.Index >= 0 && o.Index < len(docs) {
			order = append(order, o.Index)
		}
	}
	return order, nil
}

// rerankHits áp thứ tự mới cho phần đầu hits (top ≤ rerankTop); phần sau giữ nguyên.
func rerankHits(hits []Hit, order []int) []Hit {
	if len(order) < 2 {
		return hits
	}
	top := min(len(hits), rerankTop, len(order))
	head := make([]Hit, 0, top)
	for _, idx := range order[:top] {
		if idx >= 0 && idx < len(hits) {
			head = append(head, hits[idx])
		}
	}
	used := make(map[int]bool, top)
	for _, idx := range order[:top] {
		used[idx] = true
	}
	for i, h := range hits {
		if !used[i] {
			head = append(head, h)
		}
	}
	return head
}

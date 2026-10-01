package recall

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Dim khớp cột items.embedding VECTOR(1024) (bge-m3) — giống biva_worker.embed.DIM.
const Dim = 1024

// Embedder nhúng câu truy vấn. Phải cùng model với job index.items (ai-worker) để so được vector.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// TEI gọi text-embeddings-inference (CPU), cùng instance ai-worker dùng.
type TEI struct {
	URL    string
	Client *http.Client
}

func NewTEI(url string) *TEI {
	return &TEI{URL: strings.TrimRight(url, "/"), Client: &http.Client{Timeout: 5 * time.Second}}
}

func (t *TEI) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{"inputs": []string{text}, "normalize": true, "truncate": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL+"/embed", bytes.NewReader(body))
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
		return nil, fmt.Errorf("TEI trả %d", resp.StatusCode)
	}
	var out [][]float32
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out) != 1 || len(out[0]) != Dim {
		return nil, fmt.Errorf("TEI trả sai kích thước (cần 1×%d)", Dim)
	}
	return out[0], nil
}

// pgvector dạng text "[a,b,...]" — cast ::vector trong SQL, không cần thư viện pgvector.
func pgvector(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 10)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', 7, 32))
	}
	b.WriteByte(']')
	return b.String()
}

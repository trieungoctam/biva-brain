package form

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
)

func TestFormFlow(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t)
	op := fmt.Sprintf("fm%d", time.Now().UnixNano())
	pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'F')`, op)
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM operators WHERE id = $1`, op) })

	mux := http.NewServeMux()
	(&Handler{DB: pool}).Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := Create(ctx, pool, srv.URL, op, "", nil, "user:b", 0); err == nil {
		t.Fatal("không có câu hỏi phải lỗi")
	}
	c, err := Create(ctx, pool, srv.URL, op, "", []Question{
		{Topic: "pets", Question: "Có nhận <script>alert(1)</script> thú cưng không?"},
		{Topic: "fare", Question: "Trẻ em tính vé thế nào?"}}, "user:b", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.URL, srv.URL+"/f/") || len(c.URL) < len(srv.URL)+30 {
		t.Fatalf("url = %s", c.URL)
	}
	var stored string
	pool.QueryRow(ctx, `SELECT token_hash FROM forms WHERE id = $1`, c.ID).Scan(&stored)
	if strings.Contains(c.URL, stored) {
		t.Fatal("không được lưu token thô")
	}

	resp, _ := http.Get(c.URL)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(string(body), "<script>") || !strings.Contains(string(body), "&lt;script&gt;") ||
		resp.Header.Get("Referrer-Policy") != "no-referrer" || !strings.Contains(string(body), `name="a1"`) {
		t.Fatalf("GET form %d: %s", resp.StatusCode, body)
	}
	if resp, _ := http.Get(srv.URL + "/f/khongco"); resp.StatusCode != 404 {
		t.Fatalf("token sai = %d", resp.StatusCode)
	}

	// Không trả lời câu nào → 400; trả lời 1 câu → submitted + job ingest hợp lệ.
	if resp, _ := http.PostForm(c.URL, url.Values{"a0": {"  "}}); resp.StatusCode != 400 {
		t.Fatalf("trống = %d", resp.StatusCode)
	}
	resp, _ = http.PostForm(c.URL, url.Values{"a0": {"Nhận mèo nhỏ trong lồng"}, "a1": {""}})
	if resp.StatusCode != 200 {
		t.Fatalf("gửi = %d", resp.StatusCode)
	}
	st, err := Get(ctx, pool, op, c.ID)
	if err != nil || st.Status != "submitted" || len(st.Answers) != 1 || st.Answers[0].Topic != "pets" || st.OperationID == "" {
		t.Fatalf("status = %+v %v", st, err)
	}
	var payload map[string]any
	pool.QueryRow(ctx, `SELECT payload FROM operations WHERE id = $1 AND kind = 'ingest'`, st.OperationID).Scan(&payload)
	cp := jsonschema.NewCompiler()
	cp.AssertFormat()
	sch, err := cp.Compile(filepath.Join("..", "..", "..", "contracts", "schemas", "jobs", "ingest.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate(roundTrip(payload)); err != nil || !strings.Contains(payload["content"].(string), "Nhận mèo nhỏ trong lồng") {
		t.Fatalf("payload = %v (%v)", payload, err)
	}
	// Trích deterministic: mỗi câu trả lời → item policy theo topic câu hỏi (không cần LLM).
	items, _ := payload["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("payload thiếu items (đường không-LLM): %v", payload)
	}
	first, _ := items[0].(map[string]any)
	if first["kind"] != "policy" || first["topic"] == "" || !strings.HasPrefix(first["key"].(string), first["topic"].(string)+".q_") {
		t.Fatalf("item form = %v", first)
	}
	// Gửi lại → 409, vẫn một job.
	if resp, _ := http.PostForm(c.URL, url.Values{"a0": {"khác"}}); resp.StatusCode != 409 {
		t.Fatalf("gửi lại = %d", resp.StatusCode)
	}
	if _, err := Get(ctx, pool, "khac", c.ID); err != ErrNotFound {
		t.Fatalf("nhà xe khác: %v", err)
	}

	// Hết hạn → 410.
	exp, _ := Create(ctx, pool, srv.URL, op, "", []Question{{Topic: "x", Question: "?"}}, "user:b", time.Hour)
	pool.Exec(ctx, `UPDATE forms SET expires_at = now() - interval '1 minute' WHERE id = $1`, exp.ID)
	if resp, _ := http.Get(exp.URL); resp.StatusCode != 410 {
		t.Fatalf("hết hạn = %d", resp.StatusCode)
	}
}

func roundTrip(v map[string]any) any {
	out := map[string]any{}
	for k, x := range v {
		out[k] = x
	}
	return out
}

// (review q2) Item form phải: tự hiểu được (câu hỏi + trả lời, không chỉ "Đúng rồi"),
// key phân biệt hai câu cùng tiền tố, và text luôn ≥5 ký tự (schema ingest).
func TestFormItemExtractionQuality(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	op := "fq" + fmt.Sprintf("%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'F')`, op); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM operators WHERE id = $1`, op) })
	// submit form với câu xác nhận ngắn + hai câu cùng tiền tố
	mux := http.NewServeMux()
	(&Handler{DB: pool}).Mount(mux)
	fsrv := httptest.NewServer(mux)
	defer fsrv.Close()
	created, err := Create(ctx, pool, fsrv.URL, op, "", []Question{
		{Topic: "boarding", Question: "Nhà xe có áp dụng như sau không: khách nên có mặt trước 30 phút?"},
		{Topic: "boarding", Question: "Nhà xe có áp dụng như sau không: khách mang giấy tờ tuỳ thân?"},
		{Topic: "pets", Question: "Có nhận chó mèo không?"},
	}, "user:b", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Submit qua handler thật (bỏ trống câu pets → 2 item)
	if resp, err := http.PostForm(created.URL, url.Values{
		"a0": {"Đúng rồi"}, "a1": {"Có"}, "a2": {""}}); err != nil || resp.StatusCode != 200 {
		t.Fatalf("submit: %v %v", resp, err)
	}
	var payload map[string]any
	pool.QueryRow(ctx, `SELECT payload FROM operations WHERE id = (SELECT operation_id FROM forms WHERE id = $1::uuid)`,
		created.ID).Scan(&payload)
	items, _ := payload["items"].([]any)
	if len(items) != 2 { // câu pets bỏ trống → không thành item
		t.Fatalf("items = %d, muốn 2", len(items))
	}
	keys := map[string]bool{}
	for _, it := range items {
		m, _ := it.(map[string]any)
		keys[m["key"].(string)] = true
		text := m["text"].(string)
		if !strings.Contains(text, "Câu hỏi:") || !strings.Contains(text, "Trả lời:") {
			t.Fatalf("text thiếu ngữ cảnh: %q", text)
		}
		if len([]rune(text)) < 5 || len([]rune(text)) > 2000 {
			t.Fatalf("text ngoài khoảng schema: %d runes", len([]rune(text)))
		}
	}
	if len(keys) != 2 {
		t.Fatalf("hai câu cùng tiền tố phải KHÁC key: %v", keys)
	}
	for k := range keys {
		if utf8.RuneCountInString(k) > 200 {
			t.Fatalf("key vượt 200 ký tự (schema ingest): %d", len(k))
		}
	}

	// (r3) Câu hỏi DÀI không được nuốt mất câu trả lời khi clamp.
	mux2 := http.NewServeMux()
	(&Handler{DB: pool}).Mount(mux2)
	srv2 := httptest.NewServer(mux2)
	defer srv2.Close()
	// (r4) Câu hỏi >1500 rune bị Check chặn NGAY tại create — không còn đường vào
	// form để rồi bị cắt im lặng trong item.
	longQ := strings.Repeat("Quy định đặc biệt về hành lý khi đi lễ Tết ", 50) // ~2150 rune
	if utf8.RuneCountInString(longQ) <= 1500 {
		t.Fatalf("fixture phải >1500 rune, đang %d", utf8.RuneCountInString(longQ))
	}
	if _, err := Create(ctx, pool, srv2.URL, op, "", []Question{
		{Topic: "luggage", Question: longQ}}, "user:b", time.Hour); err == nil {
		t.Fatal("câu hỏi >1500 rune phải bị Check chặn")
	}

	// (r4) Key budget: câu hỏi chứa token dài (số tài khoản dán liền) → key vẫn ≤200,
	// vẫn kết thúc bằng hash 8 hex.
	mux3 := http.NewServeMux()
	(&Handler{DB: pool}).Mount(mux3)
	srv3 := httptest.NewServer(mux3)
	defer srv3.Close()
	c3, err := Create(ctx, pool, srv3.URL, op, "", []Question{
		{Topic: "payment", Question: "Số tài khoản " + strings.Repeat("12010000", 25) + " tên gì?"}}, "user:b", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := http.PostForm(c3.URL, url.Values{"a0": {"Vietcombank Chi nhánh Quận 1"}}); err != nil || resp.StatusCode != 200 {
		t.Fatalf("submit 3: %v %v", resp, err)
	}
	var payload3 map[string]any
	pool.QueryRow(ctx, `SELECT payload FROM operations WHERE id = (SELECT operation_id FROM forms WHERE id = $1::uuid)`,
		c3.ID).Scan(&payload3)
	items3, _ := payload3["items"].([]any)
	if len(items3) != 1 {
		t.Fatalf("items3 = %d", len(items3))
	}
	k3 := items3[0].(map[string]any)["key"].(string)
	if len(k3) > 200 {
		t.Fatalf("key %d > 200", len(k3))
	}
	txt3 := items3[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt3, "Vietcombank") || utf8.RuneCountInString(txt3) > 2000 {
		t.Fatalf("text3 sai: %d rune, chứa trả lời=%v", utf8.RuneCountInString(txt3), strings.Contains(txt3, "Vietcombank"))
	}
}

// Package form: form hỏi nhà xe (M2, S2.1.3). Builder tạo link (create_form), nhà xe mở trên điện thoại và trả
// lời; câu trả lời thành job ingest (source=form) → review queue. Link chứa token bí mật (chỉ lưu SHA-256), hết
// hạn sau ttl, gửi được một lần.
package form

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/textnorm"
)

const (
	DefaultTTL   = 14 * 24 * time.Hour
	MaxQuestions = 30
	maxAnswer    = 4000
)

type Question struct {
	Topic    string `json:"topic"`
	Question string `json:"question"`
}

type Answer struct {
	Topic    string `json:"topic"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

type Created struct {
	ID        string    `json:"form_id"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	Questions int       `json:"questions"`
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// Check: lỗi đầu vào (trả nguyên văn cho AI sửa).
func Check(qs []Question) error {
	if len(qs) == 0 || len(qs) > MaxQuestions {
		return fmt.Errorf("cần 1–%d câu hỏi", MaxQuestions)
	}
	for i, q := range qs {
		if strings.TrimSpace(q.Question) == "" {
			return fmt.Errorf("questions[%d]: câu hỏi trống", i)
		}
		if utf8.RuneCountInString(q.Question) > 1500 {
			return fmt.Errorf("questions[%d]: câu hỏi dài quá 1500 ký tự", i)
		}
	}
	return nil
}

// Create: form mới; URL = publicURL/f/<token>.
func Create(ctx context.Context, db *pgxpool.Pool, publicURL, operatorID, title string, qs []Question, actor string,
	ttl time.Duration) (Created, error) {
	var c Created
	if err := Check(qs); err != nil {
		return c, err
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if strings.TrimSpace(title) == "" {
		title = "Thông tin cho trợ lý chat của nhà xe"
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return c, err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	c.ExpiresAt = time.Now().Add(ttl).UTC()
	c.Questions = len(qs)
	if err := db.QueryRow(ctx, `INSERT INTO forms (operator_id, token_hash, title, questions, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id::text`, operatorID, hashToken(token), title, qs, actor, c.ExpiresAt).
		Scan(&c.ID); err != nil {
		return c, err
	}
	c.URL = strings.TrimRight(publicURL, "/") + "/f/" + token
	_, err := db.Exec(ctx, `INSERT INTO audit_log (actor, action, target, payload) VALUES ($1, 'form.create', $2, $3)`,
		actor, "form:"+c.ID, map[string]any{"operator_id": operatorID, "questions": len(qs)})
	return c, err
}

type Status struct {
	ID          string     `json:"form_id"`
	Title       string     `json:"title"`
	Status      string     `json:"status"`
	Questions   []Question `json:"questions"`
	Answers     []Answer   `json:"answers,omitempty"`
	OperationID string     `json:"operation_id,omitempty"`
	ExpiresAt   time.Time  `json:"expires_at"`
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
}

var ErrNotFound = errors.New("không tìm thấy form")

func Get(ctx context.Context, db *pgxpool.Pool, operatorID, id string) (Status, error) {
	var s Status
	var op *string
	err := db.QueryRow(ctx, `SELECT id::text, title, status, questions, answers, operation_id::text, expires_at, submitted_at
		FROM forms WHERE id = $1::uuid AND operator_id = $2`, id, operatorID).
		Scan(&s.ID, &s.Title, &s.Status, &s.Questions, &s.Answers, &op, &s.ExpiresAt, &s.SubmittedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrNotFound
	}
	if op != nil {
		s.OperationID = *op
	}
	return s, err
}

// ─────────────────────────────── trang cho nhà xe ───────────────────────────────

type Handler struct{ DB *pgxpool.Pool }

func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /f/{token}", h.show)
	mux.HandleFunc("POST /f/{token}", h.submit)
}

type formRow struct {
	id, operatorID, title, status string
	questions                     []Question
	expires                       time.Time
}

func (h *Handler) load(r *http.Request) (formRow, error) {
	var f formRow
	err := h.DB.QueryRow(r.Context(), `SELECT id::text, operator_id, title, status, questions, expires_at FROM forms
		WHERE token_hash = $1`, hashToken(r.PathValue("token"))).
		Scan(&f.id, &f.operatorID, &f.title, &f.status, &f.questions, &f.expires)
	return f, err
}

var page = template.Must(template.New("form").Funcs(template.FuncMap{"inc": func(i int) int { return i + 1 }}).Parse(`<!doctype html>
<html lang="vi"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;max-width:640px;margin:0 auto;padding:16px;color:#1f2933;background:#fff}
h1{font-size:1.3rem}label{display:block;font-weight:600;margin:18px 0 6px}
textarea{width:100%;box-sizing:border-box;min-height:84px;font:inherit;padding:8px;border:1px solid #c4cdd5;border-radius:6px}
button{margin-top:20px;width:100%;padding:12px;font:inherit;font-weight:600;background:#1565c0;color:#fff;border:0;border-radius:6px}
.note{color:#52606d;font-size:.9rem}.msg{padding:14px;border-radius:6px;background:#e3f2fd}
</style></head><body>
<h1>{{.Title}}</h1>
{{if .Message}}<p class="msg">{{.Message}}</p>{{else}}
<p class="note">Anh/chị trả lời câu nào biết được, câu nào chưa có thì để trống. Thông tin sẽ được đội BIVA kiểm tra trước khi đưa vào trợ lý chat.</p>
<form method="post">
{{range $i, $q := .Questions}}<label for="a{{$i}}">{{inc $i}}. {{$q.Question}}</label>
<textarea id="a{{$i}}" name="a{{$i}}" maxlength="4000"></textarea>
{{end}}<button type="submit">Gửi</button>
</form>{{end}}
</body></html>`))

func render(w http.ResponseWriter, status int, title, msg string, qs []Question) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer") // không lộ token qua Referer
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := page.Execute(w, map[string]any{"Title": title, "Message": msg, "Questions": qs}); err != nil {
		slog.Error("render form lỗi", "err", err)
	}
}

func (h *Handler) show(w http.ResponseWriter, r *http.Request) {
	f, err := h.load(r)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		render(w, http.StatusNotFound, "Không tìm thấy", "Link không đúng hoặc đã bị huỷ.", nil)
	case err != nil:
		slog.Error("đọc form lỗi", "err", err)
		render(w, http.StatusServiceUnavailable, "Tạm thời gián đoạn", "Anh/chị thử lại sau ít phút giúp em.", nil)
	case f.status != "open":
		render(w, http.StatusOK, f.title, "Nhà xe đã gửi câu trả lời. Cảm ơn anh/chị!", nil)
	case time.Now().After(f.expires):
		render(w, http.StatusGone, f.title, "Link đã hết hạn — anh/chị nhắn đội BIVA để nhận link mới.", nil)
	default:
		render(w, http.StatusOK, f.title, "", f.questions)
	}
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		render(w, http.StatusBadRequest, "Lỗi", "Nội dung quá dài.", nil)
		return
	}
	f, err := h.load(r)
	if errors.Is(err, pgx.ErrNoRows) {
		render(w, http.StatusNotFound, "Không tìm thấy", "Link không đúng hoặc đã bị huỷ.", nil)
		return
	}
	if err != nil {
		slog.Error("đọc form lỗi", "err", err)
		render(w, http.StatusServiceUnavailable, "Tạm thời gián đoạn", "Anh/chị thử lại sau ít phút giúp em.", nil)
		return
	}
	if f.status != "open" || time.Now().After(f.expires) {
		render(w, http.StatusConflict, f.title, "Form đã được gửi hoặc đã hết hạn.", nil)
		return
	}
	var answers []Answer
	var content strings.Builder
	for i, q := range f.questions {
		a := strings.TrimSpace(r.PostFormValue(fmt.Sprintf("a%d", i)))
		if a == "" {
			continue
		}
		if utf8.RuneCountInString(a) > maxAnswer {
			a = string([]rune(a)[:maxAnswer])
		}
		answers = append(answers, Answer{Topic: q.Topic, Question: q.Question, Answer: a})
		fmt.Fprintf(&content, "Hỏi (%s): %s\nNhà xe trả lời: %s\n\n", q.Topic, q.Question, a)
	}
	if len(answers) == 0 {
		render(w, http.StatusBadRequest, f.title, "Anh/chị chưa trả lời câu nào.", f.questions)
		return
	}
	if err := h.accept(r.Context(), f, answers, content.String()); err != nil {
		if errors.Is(err, errAlreadySubmitted) {
			render(w, http.StatusConflict, f.title, "Form đã được gửi trước đó.", nil)
			return
		}
		slog.Error("nhận form lỗi", "err", err)
		render(w, http.StatusServiceUnavailable, "Tạm thời gián đoạn", "Chưa gửi được — anh/chị thử lại sau ít phút giúp em.", nil)
		return
	}
	render(w, http.StatusOK, f.title, "Đã gửi. Cảm ơn anh/chị!", nil)
}

var errAlreadySubmitted = errors.New("form đã gửi")

// accept: ghi câu trả lời + tạo job ingest trong cùng một transaction (không có form "đã gửi" mà mất job).
func (h *Handler) accept(ctx context.Context, f formRow, answers []Answer, content string) error {
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Khoá form: hai lần bấm Gửi đồng thời → lần sau thấy đã submitted.
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM forms WHERE id = $1::uuid FOR UPDATE`, f.id).Scan(&status); err != nil {
		return err
	}
	if status != "open" {
		return errAlreadySubmitted
	}
	// Trích DETERMINISTIC: mỗi câu trả lời thành một item policy theo topic của câu hỏi
	// (câu hỏi sinh từ template nên đã gắn topic). Đường chính theo kiến trúc — form không
	// phụ thuộc GEMINI key; nội dung thô vẫn giữ trong payload.content để duyệt lại/ingest LLM
	// sau này nếu cần trích facts chi tiết hơn.
	//
	// text = CÂU HỎI + CÂU TRẢ LỜI: item tự hiểu được (câu "Đúng rồi" từng thành toàn bộ
	// policy rồi thay thông lệ L1 trong pack — review q2); đồng thời bảo đảm ≥5 ký tự của
	// schema ingest (câu trả lời 1 từ từng làm payload bị từ chối vĩnh viễn sau khi form đã
	// khóa submitted). Key = topic + 6 token + hash 8 ký tự của TOÀN BỘ câu hỏi: hai câu
	// xác nhận cùng tiền tố ("Nhà xe có áp dụng như sau không…") không còn gộp chung key.
	items := []map[string]any{}
	for _, a := range answers {
		ans := strings.TrimSpace(a.Answer)
		if ans == "" {
			continue
		}
		// Clamp theo NGÂN SÁCH ĐỘNG, ưu tiên TRẢ LỜI (nội dung nhà xe): câu trả lời chỉ bị
		// cắt khi tổng thật sự vượt 2000 rune của schema ingest; câu hỏi rút ngắn trước
		// (r3: clamp từ đầu từng cho câu hỏi dài nuốt câu trả lời; r4: clip cố định 450
		// từng cắt câu trả lời dài dù câu hỏi ngắn còn dư ngân sách).
		clip := func(s string, n int) string {
			if utf8.RuneCountInString(s) <= n {
				return s
			}
			return string([]rune(s)[:n-1]) + "…"
		}
		// Ngân sách theo công thức q5: câu hỏi được DỰNG trước min(độ dài, 200) rune —
		// ngữ cảnh luôn giữ được; câu trả lời lấy phần còn lại của 1979 (không bao giờ
		// âm, không panic ở biên 1978 như bản budget-200 cứng; không cắt câu trả lời
		// nhiều hơn mức cần khi câu hỏi ngắn).
		const budget = 2000 - 9 - 12 // "Câu hỏi: " + " → Trả lời: "
		qLen := utf8.RuneCountInString(a.Question)
		qKeep := qLen
		if qKeep > 200 {
			qKeep = 200
		}
		a_ := clip(ans, budget-qKeep)
		q := clip(a.Question, budget-utf8.RuneCountInString(a_))
		text := "Câu hỏi: " + q + " → Trả lời: " + a_
		toks := textnorm.Tokens(a.Question)
		if len(toks) > 6 {
			toks = toks[:6]
		}
		// Ngân sách key ≤200 ký tự của schema ingest; hash 8 hex của toàn bộ câu hỏi
		// LUÔN giữ (phân biệt hai câu cùng tiền tố) — token chỉ lấp chỗ còn dư (r4).
		sum := sha256.Sum256([]byte(a.Question))
		head := a.Topic + ".q_"
		for _, t := range toks {
			if len(head)+len(t)+9 > 200 { // 9 = "_" + 8 hex
				break
			}
			head += t + "_"
		}
		key := fmt.Sprintf("%s%x", head, sum[:4])
		items = append(items, map[string]any{
			"kind": "policy", "topic": a.Topic, "key": key, "text": text,
		})
	}
	payload := map[string]any{"operator_id": f.operatorID, "source": "form", "content": content,
		"received_at": time.Now().UTC().Format(time.RFC3339), "submitted_by": "form:" + f.id,
		"items": items}
	var opID string
	if err := tx.QueryRow(ctx, `INSERT INTO operations (kind, operator_id, payload, idempotency_key)
		VALUES ('ingest', $1, $2, $3) RETURNING id::text`, f.operatorID, payload, "form:"+f.id).Scan(&opID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE forms SET status = 'submitted', answers = $2, submitted_at = now(), operation_id = $3::uuid
		WHERE id = $1::uuid AND status = 'open'`, f.id, answers, opID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errAlreadySubmitted
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log (actor, action, target, payload) VALUES ($1, 'form.submit', $2, $3)`,
		"form:"+f.id, "form:"+f.id, map[string]any{"operator_id": f.operatorID, "answers": len(answers),
			"operation_id": opID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

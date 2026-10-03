// Package artifact lưu các phần của bot do AI viết (M1, S1.5.1): persona, system_prompt, faq, flows, tool_spec,
// fallbacks. Mỗi lần lưu là version mới (không ghi đè). Trích dẫn [[item_id]] được tách theo dòng vào
// artifact_citations — cơ sở cho validate (UNCITED, STALE_CITATION...) và mark_stale khi tri thức đổi.
//
// Bot xác định theo kênh: <operator_id>:<channel>, tự tạo khi lưu artifact đầu tiên.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	Kinds    = []string{"persona", "system_prompt", "faq", "flows", "tool_spec", "fallbacks"}
	Channels = []string{"zalo", "messenger", "web"}

	ErrNotFound = errors.New("không tìm thấy artifact")
	ErrConflict = errors.New("đã có version mới hơn")
)

const MaxContent = 100000

// citeRe: [[<uuid>]] — id item của Brain. Bắt cả dạng sai để báo lỗi theo dòng.
var citeRe = regexp.MustCompile(`\[\[\s*([^\[\]\s]+)\s*\]\]`)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type Citation struct {
	ItemID string `json:"item_id"`
	Line   int    `json:"line"`
}

// ParseCitations: các trích dẫn theo dòng (1-based) + lỗi cú pháp (id không phải UUID).
func ParseCitations(content string) ([]Citation, []string) {
	var cites []Citation
	var errs []string
	for i, line := range strings.Split(content, "\n") {
		for _, m := range citeRe.FindAllStringSubmatch(line, -1) {
			if !uuidRe.MatchString(m[1]) {
				errs = append(errs, fmt.Sprintf("dòng %d: [[%s]] không phải id item (UUID)", i+1, m[1]))
				continue
			}
			cites = append(cites, Citation{ItemID: strings.ToLower(m[1]), Line: i + 1})
		}
	}
	return cites, errs
}

func BotID(operatorID, channel string) string { return operatorID + ":" + channel }

type SaveInput struct {
	OperatorID       string
	Channel          string
	Kind             string
	Content          string
	Note             string
	BaseVersion      int // > 0: chỉ lưu nếu version hiện tại đúng bằng số này (tránh ghi đè bản người khác vừa lưu)
	Author           string
	KnowledgeVersion string
}

type Warning struct {
	Line   int    `json:"line"`
	ItemID string `json:"item_id"`
	Status string `json:"status"` // superseded | expired | retracted | ...
	Note   string `json:"note"`
}

type SaveResult struct {
	ID        string    `json:"id"`
	BotID     string    `json:"bot_id"`
	Kind      string    `json:"kind"`
	Version   int       `json:"version"`
	Status    string    `json:"status"`
	Unchanged bool      `json:"unchanged"` // nội dung y hệt version hiện tại → không tạo version mới
	Citations int       `json:"citations"`
	Warnings  []Warning `json:"warnings,omitempty"`
}

// InputError: lỗi do nội dung AI gửi (trả nguyên văn để AI sửa), khác lỗi hệ thống.
type InputError struct{ Problems []string }

func (e *InputError) Error() string { return strings.Join(e.Problems, "; ") }

func Save(ctx context.Context, db *pgxpool.Pool, in SaveInput) (SaveResult, error) {
	var res SaveResult
	if in.Channel == "" {
		in.Channel = "zalo"
	}
	var problems []string
	if !slices.Contains(Kinds, in.Kind) {
		problems = append(problems, "kind phải là: "+strings.Join(Kinds, ", "))
	}
	if !slices.Contains(Channels, in.Channel) {
		problems = append(problems, "channel phải là: "+strings.Join(Channels, ", "))
	}
	content := strings.TrimSpace(strings.ReplaceAll(in.Content, "\r\n", "\n"))
	if content == "" || utf8.RuneCountInString(content) > MaxContent {
		problems = append(problems, fmt.Sprintf("content phải có nội dung và ≤ %d ký tự", MaxContent))
	}
	cites, citeErrs := ParseCitations(content)
	problems = append(problems, citeErrs...)
	if len(problems) > 0 {
		return res, &InputError{problems}
	}

	// Item được trích dẫn phải tồn tại và thuộc phạm vi (L0/L1 hoặc của chính nhà xe này).
	ids := uniqueIDs(cites)
	status := map[string]string{}
	superseded := map[string]string{}
	if len(ids) > 0 {
		rows, err := db.Query(ctx, `SELECT id::text, status, COALESCE(superseded_by::text, '') FROM items
			WHERE id = ANY($1::uuid[]) AND (layer <= 1 OR operator_id = $2)`, ids, in.OperatorID)
		if err != nil {
			return res, err
		}
		for rows.Next() {
			var id, st, sup string
			if err := rows.Scan(&id, &st, &sup); err != nil {
				rows.Close()
				return res, err
			}
			status[id], superseded[id] = st, sup
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, err
		}
	}
	for _, c := range cites {
		if _, ok := status[c.ItemID]; !ok {
			problems = append(problems, fmt.Sprintf("dòng %d: [[%s]] không phải item của nhà xe này hoặc tri thức nền",
				c.Line, c.ItemID))
		}
	}
	if len(problems) > 0 {
		return res, &InputError{problems}
	}
	for _, c := range cites {
		if st := status[c.ItemID]; st != "active" {
			note := "item không còn hiệu lực; tìm bản thay thế bằng recall_knowledge"
			if s := superseded[c.ItemID]; s != "" {
				note = "đã được thay bằng [[" + s + "]]"
			}
			res.Warnings = append(res.Warnings, Warning{Line: c.Line, ItemID: c.ItemID, Status: st, Note: note})
		}
	}

	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	botID := BotID(in.OperatorID, in.Channel)

	tx, err := db.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO bots (id, operator_id, channel) VALUES ($1, $2, $3) ON CONFLICT (id) DO NOTHING`,
		botID, in.OperatorID, in.Channel); err != nil {
		return res, err
	}
	// Khoá bot: các lần lưu đồng thời của cùng bot xếp hàng, version không trùng.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM bots WHERE id = $1 FOR UPDATE`, botID); err != nil {
		return res, err
	}
	var curID, curHash, curStatus string
	var curVersion int
	err = tx.QueryRow(ctx, `SELECT id::text, version, content_hash, status FROM bot_artifacts
		WHERE bot_id = $1 AND kind = $2 ORDER BY version DESC LIMIT 1`, botID, in.Kind).
		Scan(&curID, &curVersion, &curHash, &curStatus)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return res, err
	}
	res.BotID, res.Kind, res.Citations = botID, in.Kind, len(cites)
	if curHash == hash {
		res.ID, res.Version, res.Status, res.Unchanged = curID, curVersion, curStatus, true
		return res, nil
	}
	if in.BaseVersion > 0 && in.BaseVersion != curVersion {
		if curVersion == 0 {
			// Kind này CHƯA TỪNG có artifact — nói thẳng thay vì "v0 mới hơn v1" gây lạc hướng.
			return res, fmt.Errorf("chưa có artifact %s nào của bot này — bỏ base_version để tạo bản đầu", in.Kind)
		}
		return res, fmt.Errorf("%w (v%d, bạn sửa từ v%d) — get_artifact bản mới rồi sửa lại trên đó", ErrConflict,
			curVersion, in.BaseVersion)
	}
	res.Version, res.Status = curVersion+1, "draft"
	if err := tx.QueryRow(ctx, `INSERT INTO bot_artifacts (bot_id, operator_id, kind, version, content, content_hash,
			author, note, knowledge_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, '')) RETURNING id::text`,
		botID, in.OperatorID, in.Kind, res.Version, content, hash, in.Author, strings.TrimSpace(in.Note),
		in.KnowledgeVersion).Scan(&res.ID); err != nil {
		return res, err
	}
	if len(cites) > 0 {
		rows := make([][]any, 0, len(cites))
		seen := map[Citation]bool{}
		for _, c := range cites {
			if !seen[c] {
				seen[c] = true
				rows = append(rows, []any{res.ID, c.ItemID, c.Line})
			}
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"artifact_citations"}, []string{"artifact_id", "item_id", "line"},
			pgx.CopyFromRows(rows)); err != nil {
			return res, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log (actor, action, target, payload) VALUES ($1, 'artifact.save', $2, $3)`,
		in.Author, "artifact:"+res.ID, map[string]any{"operator_id": in.OperatorID, "bot_id": botID, "kind": in.Kind,
			"version": res.Version, "citations": len(cites), "note": in.Note}); err != nil {
		return res, err
	}
	return res, tx.Commit(ctx)
}

func uniqueIDs(cites []Citation) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range cites {
		if !seen[c.ItemID] {
			seen[c.ItemID] = true
			out = append(out, c.ItemID)
		}
	}
	return out
}

type Artifact struct {
	ID               string           `json:"id"`
	BotID            string           `json:"bot_id"`
	Kind             string           `json:"kind"`
	Version          int              `json:"version"`
	Status           string           `json:"status"`
	Author           string           `json:"author"`
	Note             string           `json:"note,omitempty"`
	KnowledgeVersion string           `json:"knowledge_version,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`
	Content          string           `json:"content"`
	Citations        []Citation       `json:"citations"`
	Validation       map[string]any   `json:"validation,omitempty"`
	Versions         []VersionSummary `json:"versions"` // lịch sử, mới nhất trước
}

type VersionSummary struct {
	Version   int       `json:"version"`
	Status    string    `json:"status"`
	Author    string    `json:"author"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Get: một version (0 = mới nhất) của artifact.
func Get(ctx context.Context, db *pgxpool.Pool, operatorID, channel, kind string, version int) (Artifact, error) {
	var a Artifact
	if channel == "" {
		channel = "zalo"
	}
	botID := BotID(operatorID, channel)
	var note, kv *string
	err := db.QueryRow(ctx, `SELECT id::text, bot_id, kind, version, status, author, note, knowledge_version, created_at,
			content, validation
		FROM bot_artifacts WHERE operator_id = $1 AND bot_id = $2 AND kind = $3 AND ($4 = 0 OR version = $4)
		ORDER BY version DESC LIMIT 1`, operatorID, botID, kind, version).
		Scan(&a.ID, &a.BotID, &a.Kind, &a.Version, &a.Status, &a.Author, &note, &kv, &a.CreatedAt, &a.Content, &a.Validation)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.Note, a.KnowledgeVersion = deref(note), deref(kv)
	rows, err := db.Query(ctx, `SELECT item_id::text, line FROM artifact_citations WHERE artifact_id = $1 ORDER BY line, item_id`, a.ID)
	if err != nil {
		return a, err
	}
	a.Citations = []Citation{}
	for rows.Next() {
		var c Citation
		if err := rows.Scan(&c.ItemID, &c.Line); err != nil {
			rows.Close()
			return a, err
		}
		a.Citations = append(a.Citations, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return a, err
	}
	rows, err = db.Query(ctx, `SELECT version, status, author, COALESCE(note, ''), created_at FROM bot_artifacts
		WHERE bot_id = $1 AND kind = $2 ORDER BY version DESC LIMIT 50`, botID, kind)
	if err != nil {
		return a, err
	}
	defer rows.Close()
	for rows.Next() {
		var v VersionSummary
		if err := rows.Scan(&v.Version, &v.Status, &v.Author, &v.Note, &v.CreatedAt); err != nil {
			return a, err
		}
		a.Versions = append(a.Versions, v)
	}
	return a, rows.Err()
}

type Summary struct {
	BotID     string    `json:"bot_id"`
	Channel   string    `json:"channel"`
	Kind      string    `json:"kind"`
	Version   int       `json:"version"`
	Status    string    `json:"status"`
	Citations int       `json:"citations"`
	UpdatedAt time.Time `json:"updated_at"`
}

// List: version mới nhất của mỗi artifact của nhà xe (mọi kênh, hoặc một kênh).
func List(ctx context.Context, db *pgxpool.Pool, operatorID, channel string) ([]Summary, error) {
	rows, err := db.Query(ctx, `SELECT DISTINCT ON (a.bot_id, a.kind) a.bot_id, b.channel, a.kind, a.version, a.status,
			(SELECT count(*) FROM artifact_citations c WHERE c.artifact_id = a.id), a.created_at
		FROM bot_artifacts a JOIN bots b ON b.id = a.bot_id
		WHERE a.operator_id = $1 AND ($2 = '' OR b.channel = $2)
		ORDER BY a.bot_id, a.kind, a.version DESC`, operatorID, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var s Summary
		if err := rows.Scan(&s.BotID, &s.Channel, &s.Kind, &s.Version, &s.Status, &s.Citations, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

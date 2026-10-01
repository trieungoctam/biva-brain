package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/trieungoctam/biva-brain/brain-api/internal/textnorm"
)

func TestRecallTools(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, err := connect(t, f.url+"/mcp/operator/"+f.opA+"/", f.builderTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	insert := func(op, kind, topic, key, text, value, validFrom, validTo string) string {
		var id string
		var vf, vt any
		if validFrom != "" {
			vf = validFrom
		}
		if validTo != "" {
			vt = validTo
		}
		var val any
		if value != "" {
			val = value
		}
		if err := f.pool.QueryRow(ctx, `INSERT INTO items (layer, operator_id, kind, topic, key, text, value, status,
				search_text, valid_from, valid_to)
			VALUES (2, $1, $2, $3, $4, $5, $6::jsonb, 'active', $7, $8::timestamptz, $9::timestamptz) RETURNING id::text`,
			op, kind, topic, key, text, val, textnorm.SearchText(topic+" "+key+" "+text), vf, vt).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	pets := insert(f.opA, "policy", "pets", "pets.cho_meo", "Nhà xe không nhận chó mèo lên xe", "", "", "")
	insert(f.opB, "policy", "pets", "pets.cho_meo", "Nhà xe B nhận chó mèo", "", "", "")
	fare := insert(f.opA, "data", "fare", "fare.sg_dl", "Sài Gòn → Đà Lạt 320.000đ",
		`{"facts": {"diem_den": "Đà Lạt", "gia_ve": "320000"}}`, "", "2099-11-30T23:59:59+07:00")
	insert(f.opA, "data", "fare", "fare.sg_dl", "Sài Gòn → Đà Lạt 350.000đ từ 01/12/2099",
		`{"facts": {"diem_den": "Đà Lạt", "gia_ve": "350000"}}`, "2099-12-01T00:00:00+07:00", "")

	// recall_knowledge: không dấu vẫn khớp; chỉ nhà xe A; có nhãn tầng; báo thiếu TEI.
	isErr, out, text := call(t, s, "recall_knowledge", map[string]any{"query": "cho meo", "topics": []string{"pets"}})
	if isErr {
		t.Fatal(text)
	}
	items := out["items"].([]any)
	first := items[0].(map[string]any)
	if first["id"] != pets || first["layer"] != "L2" || first["label"] != "nhà xe" || out["degraded"] == nil {
		t.Fatalf("recall = %v", out)
	}
	for _, it := range items {
		if strings.Contains(it.(map[string]any)["text"].(string), "Nhà xe B") {
			t.Fatal("lộ tri thức nhà xe khác")
		}
	}
	if isErr, _, text := call(t, s, "recall_knowledge", map[string]any{"query": "x", "topics": []string{"karaoke"}}); !isErr ||
		!strings.Contains(text, "topic không hợp lệ: karaoke") {
		t.Fatalf("topic sai: %v %s", isErr, text)
	}
	if isErr, _, text := call(t, s, "recall_knowledge", map[string]any{"valid_at": "1/11"}); !isErr ||
		!strings.Contains(text, "valid_at phải dạng YYYY-MM-DD") {
		t.Fatalf("ngày sai: %v %s", isErr, text)
	}

	// query_data: giá hiện tại; bản 2099 nằm ở upcoming; tra đúng ngày 2099-12-01 thì ra giá mới.
	_, out, _ = call(t, s, "query_data", map[string]any{"topics": []string{"fare"}, "match": "da lat", "include_upcoming": true})
	rows, up := out["items"].([]any), out["upcoming"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["id"] != fare || len(up) != 1 {
		t.Fatalf("query_data = %v", out)
	}
	_, out, _ = call(t, s, "query_data", map[string]any{"facts": map[string]string{"diem_den": "đà lạt"}, "date": "2099-12-01"})
	if rows := out["items"].([]any); len(rows) != 1 || rows[0].(map[string]any)["facts"].(map[string]any)["gia_ve"] != "350000" {
		t.Fatalf("query_data ngày 2099-12-01 = %v", out)
	}

	// get_source: nhận cả dạng trích dẫn [[id]].
	isErr, out, text = call(t, s, "get_source", map[string]any{"item_id": "[[" + pets + "]]"})
	if isErr || out["item"].(map[string]any)["id"] != pets {
		t.Fatalf("get_source = %v %s", out, text)
	}

	// get_operator_overview: fare (bắt buộc) đã có.
	isErr, out, text = call(t, s, "get_operator_overview", map[string]any{})
	if isErr {
		t.Fatal(text)
	}
	cov := out["coverage"].(map[string]any)
	if out["operator"].(map[string]any)["id"] != f.opA || len(cov["missing_required"].([]any)) != 0 ||
		cov["required"].([]any)[0].(map[string]any)["active"].(float64) != 2 {
		t.Fatalf("overview = %v", out)
	}
}

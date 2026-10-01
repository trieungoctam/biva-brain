package entity

import (
	"strings"
	"testing"
)

var dict = []Entity{
	{ID: "hcm", Type: "city", Name: "TP.HCM", Aliases: []string{"Sài Gòn", "SG", "Hồ Chí Minh", "thành phố Hồ Chí Minh"}},
	{ID: "da_lat", Type: "city", Name: "Đà Lạt"},
	{ID: "bx_mien_dong_moi", Type: "station", Name: "Bến xe Miền Đông mới", Aliases: []string{"Miền Đông mới"}},
	{ID: "giuong_nam", Type: "vehicle_type", Name: "giường nằm", Aliases: []string{"xe giường nằm"}},
}

func TestFind(t *testing.T) {
	r, err := New(dict)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(text string) string {
		var out []string
		for _, m := range r.Find(text) {
			out = append(out, m.Entity.ID)
		}
		return strings.Join(out, ",")
	}
	cases := map[string]string{
		"SG đi Đà Lạt":                    "hcm,da_lat",
		"sài gòn - đà lạt":                "hcm,da_lat",
		"TP.HCM ⇄ Dalat":                  "hcm",
		"Thành phố Hồ Chí Minh":           "hcm", // cụm dài nhất
		"đón ở Bến xe Miền Đông mới":      "bx_mien_dong_moi",
		"bến xe miền đông cũ":             "", // không có trong từ điển: không đoán
		"xe giường nằm 40 chỗ":            "giuong_nam",
		"sgx không phải sài gòn mà là SG": "hcm,hcm",
		"Hồ Chí":                          "",
	}
	for text, want := range cases {
		if got := ids(text); got != want {
			t.Errorf("Find(%q) = %q, muốn %q", text, got, want)
		}
	}
	if q := strings.Join(r.QueryTerms("SG đà lạt giường nằm"), " "); q != "@hcm @da_lat @giuong_nam" {
		t.Fatalf("QueryTerms = %q", q)
	}
	terms := strings.Join(r.Terms("Sài Gòn (BX Miền Đông mới) ⇄ Đà Lạt"), " ")
	for _, want := range []string{"@hcm", "@da_lat", "@bx_mien_dong_moi", "mien", "dong"} {
		if !strings.Contains(" "+terms+" ", " "+want+" ") {
			t.Fatalf("Terms thiếu %s: %s", want, terms)
		}
	}
	if _, err := New(append(dict, Entity{ID: "x", Name: "Sài gòn"})); err == nil {
		t.Fatal("alias trùng giữa hai thực thể phải lỗi")
	}
	var nilR *Resolver
	if nilR.Find("SG") != nil {
		t.Fatal("resolver nil phải an toàn")
	}
}

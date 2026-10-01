// Package entity: nhận diện thực thể theo từ điển alias L1 (M2, S2.2.1) — "SG", "Sài Gòn", "TP.HCM" → một thực thể.
// So khớp trên token textnorm (không dấu, không phân biệt hoa thường), ưu tiên cụm dài nhất. Dùng để mở rộng
// query của recall và so khớp query_data. Trigram / co-occurrence (thực thể chưa có trong từ điển): sau.
package entity

import (
	"fmt"
	"strings"

	"github.com/trieungoctam/biva-brain/brain-api/internal/textnorm"
)

type Entity struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
}

type Match struct {
	Entity     Entity
	Start, End int // vị trí token [Start, End)
}

type variant struct {
	tokens []string
	entity int
}

// Resolver tra thực thể theo token đầu của mỗi biến thể (tên + alias).
type Resolver struct {
	entities []Entity
	byFirst  map[string][]variant
}

// New dựng resolver; lỗi nếu một biến thể (sau chuẩn hoá) thuộc hai thực thể.
func New(entities []Entity) (*Resolver, error) {
	r := &Resolver{entities: entities, byFirst: map[string][]variant{}}
	owner := map[string]string{}
	var problems []string
	for i, e := range entities {
		for _, v := range append([]string{e.Name}, e.Aliases...) {
			toks := textnorm.Tokens(v)
			if len(toks) == 0 {
				continue
			}
			key := strings.Join(toks, " ")
			if prev, ok := owner[key]; ok && prev != e.ID {
				problems = append(problems, fmt.Sprintf("alias %q thuộc cả %s và %s", v, prev, e.ID))
				continue
			}
			if _, ok := owner[key]; ok {
				continue
			}
			owner[key] = e.ID
			r.byFirst[toks[0]] = append(r.byFirst[toks[0]], variant{tokens: toks, entity: i})
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("từ điển thực thể: %s", strings.Join(problems, "; "))
	}
	return r, nil
}

// FindTokens: thực thể trong dãy token (đã chuẩn hoá), cụm dài nhất trước, không chồng nhau.
func (r *Resolver) FindTokens(toks []string) []Match {
	if r == nil {
		return nil
	}
	var out []Match
	for i := 0; i < len(toks); {
		best, bestLen := -1, 0
		for _, v := range r.byFirst[toks[i]] {
			n := len(v.tokens)
			if n > bestLen && i+n <= len(toks) && equal(toks[i:i+n], v.tokens) {
				best, bestLen = v.entity, n
			}
		}
		if best < 0 {
			i++
			continue
		}
		out = append(out, Match{Entity: r.entities[best], Start: i, End: i + bestLen})
		i += bestLen
	}
	return out
}

func (r *Resolver) Find(text string) []Match { return r.FindTokens(textnorm.Tokens(text)) }

// Variants: mọi cách viết (tên + alias) của thực thể.
func (e Entity) Variants() []string { return append([]string{e.Name}, e.Aliases...) }

// Terms: tập "từ" để so khớp — token thường + "@<id>" cho mỗi thực thể nhận ra. Giữ cả token thường để
// cụm không có trong từ điển ("miền đông") vẫn khớp được.
func (r *Resolver) Terms(text string) []string {
	toks := textnorm.Tokens(text)
	out := append([]string{}, toks...)
	for _, m := range r.FindTokens(toks) {
		out = append(out, "@"+m.Entity.ID)
	}
	return out
}

// QueryTerms: yêu cầu của câu tra — phần là thực thể thay bằng "@<id>" (khớp mọi cách viết), phần còn lại là token.
func (r *Resolver) QueryTerms(text string) []string {
	toks := textnorm.Tokens(text)
	var out []string
	i := 0
	for _, m := range r.FindTokens(toks) {
		out = append(out, toks[i:m.Start]...)
		out = append(out, "@"+m.Entity.ID)
		i = m.End
	}
	return append(out, toks[i:]...)
}

func equal(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

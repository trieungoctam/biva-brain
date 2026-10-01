// Package textnorm chuẩn hoá tiếng Việt cho tìm kiếm — spec: contracts/textnorm/README.md.
//
// Phải cho kết quả giống hệt bản Python (biva_worker/textnorm.py), kiểm bằng contracts/textnorm/cases.jsonl:
// worker ghi search_text cho item, Go chuẩn hoá query theo cùng thuật toán.
package textnorm

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

func foldChars(s string) []rune {
	out := make([]rune, 0, len(s))
	for _, r := range norm.NFKD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case r == 'đ' || r == 'Đ' || r == 'ð' || r == 'Ð':
			r = 'd'
		case 'A' <= r && r <= 'Z':
			r += 'a' - 'A'
		}
		out = append(out, r)
	}
	return out
}

func isDigit(r rune) bool     { return '0' <= r && r <= '9' }
func isTokenChar(r rune) bool { return 'a' <= r && r <= 'z' || isDigit(r) }

// isThousandSep: '.'/',' sau một chữ số, theo sau đúng 3 chữ số rồi hết chuỗi hoặc ký tự không phải số.
func isThousandSep(s []rune, i int) bool {
	if s[i] != '.' && s[i] != ',' {
		return false
	}
	if i == 0 || !isDigit(s[i-1]) || i+3 >= len(s) {
		return false
	}
	for k := 1; k <= 3; k++ {
		if !isDigit(s[i+k]) {
			return false
		}
	}
	return i+4 == len(s) || !isDigit(s[i+4])
}

// Tokens trả về các token đã chuẩn hoá.
func Tokens(s string) []string {
	rs := foldChars(s)
	var toks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for i, r := range rs {
		if isThousandSep(rs, i) {
			continue
		}
		if isTokenChar(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return toks
}

// Fold: token nối bằng một dấu cách.
func Fold(s string) string { return strings.Join(Tokens(s), " ") }

// SearchText: Fold + bigram "a_b" của các token liên tiếp.
func SearchText(s string) string {
	toks := Tokens(s)
	if len(toks) < 2 {
		return strings.Join(toks, " ")
	}
	parts := append([]string{}, toks...)
	for i := 0; i+1 < len(toks); i++ {
		parts = append(parts, toks[i]+"_"+toks[i+1])
	}
	return strings.Join(parts, " ")
}

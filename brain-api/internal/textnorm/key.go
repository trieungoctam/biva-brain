package textnorm

import "strings"

// NormalizeKey chuẩn hoá key của item: các phần tách bằng "."; mỗi phần = token textnorm nối bằng "_";
// luôn bắt đầu bằng topic. Phải giống hệt biva_worker.ingest.extract.normalize_key (fixture keys.jsonl).
func NormalizeKey(topic, key string) string {
	var parts []string
	for _, raw := range strings.Split(key, ".") {
		if seg := strings.Join(Tokens(strings.ReplaceAll(raw, "_", " ")), "_"); seg != "" {
			parts = append(parts, seg)
		}
	}
	if len(parts) == 0 || parts[0] != topic {
		parts = append([]string{topic}, parts...)
	}
	return strings.Join(parts, ".")
}

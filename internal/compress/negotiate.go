package compress

import (
	"math"
	"strconv"
	"strings"
)

// Negotiate 解析客户端 Accept-Encoding 头, 按服务端 Preference 偏好序返回
// 第一个客户端可接受的编码; 头缺失或全部不可接受时返回 Identity(不压缩)。
//
// 按 RFC 9110 质量值语义实现:
//   - 省略 q 值默认 1; q=0 是明确拒绝, 且拒绝优先于通配;
//   - "*" 匹配所有未显式列出的编码;
//   - token 比较忽略大小写与空白, x-gzip 视作 gzip;
//   - 同一编码重复出现时取最小 q, 客户端表达矛盾时不给压缩;
//   - q 值非法或越界视作拒绝该编码;
//   - identity 的拒绝与调权不处理: 协商不中时返回不压缩结果恒可行。
func Negotiate(header string) string {
	accepted, wildcard := parseAcceptEncoding(header)
	for _, encoding := range Preference {
		if q, ok := accepted[encoding]; ok {
			if q > 0 {
				return encoding
			}
			continue // 显式 q=0 拒绝该编码, 通配救不回。
		}
		if wildcard > 0 {
			return encoding
		}
	}
	return Identity
}

// parseAcceptEncoding 把 Accept-Encoding 解析为 {编码: q} 表与通配 q。
// wildcard 为负表示头中不含 "*", 便于与通配 q=0 区分。
func parseAcceptEncoding(header string) (accepted map[string]float64, wildcard float64) {
	wildcard = -1
	for entry := range strings.SplitSeq(header, ",") {
		fields := strings.Split(entry, ";")
		token := normalize(fields[0])
		if token == "" || token == Identity {
			continue // 空段与 identity 不参与编码协商。
		}

		// 逐个参数找 q(参数名不区分大小写); q 缺省为 1, 取最后一个合法值。
		q := 1.0
		valid := true
		for _, field := range fields[1:] {
			name, value, found := strings.Cut(field, "=")
			if !found || normalize(name) != "q" {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || parsed < 0 || parsed > 1 {
				valid = false
				continue
			}
			q = parsed
		}
		if !valid {
			q = 0 // 非法 q 值按拒绝处理, 不猜测客户端意图。
		}

		if token == "*" {
			if wildcard < 0 {
				wildcard = q
			} else {
				wildcard = math.Min(wildcard, q)
			}
			continue
		}
		if prev, ok := accepted[token]; ok {
			accepted[token] = math.Min(prev, q)
			continue
		}
		if accepted == nil {
			accepted = make(map[string]float64, 1)
		}
		accepted[token] = q
	}
	return accepted, wildcard
}

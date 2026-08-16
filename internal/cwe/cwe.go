// Package cwe 是 CWE 编号白名单与归一化(docs/phase2/03 §1.1/§1.2)。
//
// 服务端校验面(本包,确定性):
//   - Known:白名单编号集合(OWASP Top 10 + CWE Top 25 常用子集,按需扩展)
//   - NormalizeCWE:大小写/格式容忍(cwe-89 / CWE89 / 89 → CWE-89)
//   - NormalizeAsset / NormalizeEndpoint:结构化键归一化(幂等键双轨的地基)
//
// LLM 参考面(层 3 知识,热插拔):skills/cwe-reference/SKILL.md,
// 与服务端白名单同源——skill 卡引用的编号必须都在本包 Known 中
// (builtincontent 测试防漂移)。
//
// 校验纪律(03 §1.1):错值拒字段不拒整条——cwe 不合法 → 该字段置空,
// finding 退回幂等键轨 2(逐字),事件层提示正确格式。
package cwe

import (
	"net/url"
	"regexp"
	"strings"
)

// Known 是服务端白名单:编号 → 官方名称。
// 覆盖 OWASP Top 10 2021 与 CWE Top 25 常用条目 + 高频 Web 漏洞。
var Known = map[string]string{
	"CWE-22":   "Path Traversal",
	"CWE-78":   "OS Command Injection",
	"CWE-79":   "Cross-site Scripting (XSS)",
	"CWE-80":   "HTML Injection",
	"CWE-89":   "SQL Injection",
	"CWE-90":   "LDAP Injection",
	"CWE-91":   "XML Injection",
	"CWE-93":   "CRLF Injection",
	"CWE-94":   "Code Injection",
	"CWE-99":   "Resource Injection",
	"CWE-116":  "Output Encoding Issue",
	"CWE-117":  "Log Injection",
	"CWE-120":  "Buffer Overflow",
	"CWE-190":  "Integer Overflow",
	"CWE-200":  "Information Exposure",
	"CWE-201":  "Sensitive Data in URL",
	"CWE-209":  "Error Message Information Leak",
	"CWE-269":  "Improper Privilege Management",
	"CWE-285":  "Improper Authorization",
	"CWE-287":  "Authentication Bypass",
	"CWE-295":  "Certificate Validation Issue",
	"CWE-306":  "Missing Authentication",
	"CWE-307":  "Brute Force",
	"CWE-311":  "Missing Encryption",
	"CWE-319":  "Cleartext Transmission",
	"CWE-326":  "Weak Key",
	"CWE-327":  "Broken Crypto",
	"CWE-352":  "CSRF",
	"CWE-384":  "Session Fixation",
	"CWE-400":  "Resource Exhaustion",
	"CWE-404":  "Improper Resource Shutdown",
	"CWE-425":  "Forced Browsing",
	"CWE-434":  "Unrestricted File Upload",
	"CWE-444":  "HTTP Request Smuggling",
	"CWE-502":  "Insecure Deserialization",
	"CWE-521":  "Weak Password Requirements",
	"CWE-522":  "Insufficiently Protected Credentials",
	"CWE-532":  "Sensitive Log Information",
	"CWE-548":  "Directory Listing",
	"CWE-601":  "Open Redirect",
	"CWE-611":  "XXE",
	"CWE-613":  "Session Expiration Issue",
	"CWE-620":  "Unverified Password Change",
	"CWE-639":  "IDOR",
	"CWE-640":  "Weak Password Recovery",
	"CWE-643":  "XPath Injection",
	"CWE-754":  "Unchecked Return Value",
	"CWE-759":  "Unsalted Hash",
	"CWE-770":  "Uncontrolled Resource Allocation",
	"CWE-798":  "Hard-coded Credentials",
	"CWE-862":  "Missing Authorization",
	"CWE-863":  "Incorrect Authorization",
	"CWE-916":  "Weak Hash",
	"CWE-918":  "SSRF",
	"CWE-1236": "CSV Injection",
	"CWE-1336": "Template Injection (SSTI)",
}

// IsKnown 判断归一化编号是否在白名单。
func IsKnown(norm string) bool {
	_, ok := Known[norm]
	return ok
}

var cweNumRe = regexp.MustCompile(`^[cC][wW][eE]-?(\d{1,4})$`)

// NormalizeCWE 归一化编号并校验白名单。
// 容忍 cwe-89 / CWE89 / 89 / CWE-089 等写法;返回规范形式 CWE-N。
// 不在白名单 → ("" , false)(调用方拒字段不拒整条)。
func NormalizeCWE(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	m := cweNumRe.FindStringSubmatch(s)
	if m == nil {
		// 裸数字也容忍("89")
		if isAllDigits(s) {
			m = []string{s, s}
		} else {
			return "", false
		}
	}
	num := strings.TrimLeft(m[1], "0")
	if num == "" {
		num = "0"
	}
	norm := "CWE-" + num
	if !IsKnown(norm) {
		return "", false
	}
	return norm, true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var assetPrefixRe = regexp.MustCompile(`^(?i)(https?|ftp)://`)

// methodPrefixRe 匹配 HTTP 方法前缀(模型常把 "GET /path" 整串当 endpoint 提交)。
var methodPrefixRe = regexp.MustCompile(`^(?i)(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\s+`)

// NormalizeAsset 归一化资产标识(03 §1.1):小写、去 www.、去协议、去尾部斜杠。
// 输入可以是 URL 或裸域名/IP;端口保留(多端口资产常见)。
func NormalizeAsset(s string) string {
	s = strings.TrimSpace(s)
	s = assetPrefixRe.ReplaceAllString(s, "")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	// 去 userinfo@ 前缀(URL 带凭据时)
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.ToLower(s)
	s = strings.TrimPrefix(s, "www.")
	return strings.TrimSuffix(s, "/")
}

// NormalizeEndpoint 归一化端点(03 §1.1):去方法前缀、去协议 host 只留 path、
// 去 query、去尾部斜杠、decode。空输入返回空串(可空列)。
// 输入形态兼容:裸路径 "/rest/products/search?q="、"GET /rest/products/search?q="
// (模型常见错误)、完整 URL "http://host:port/rest/products/search?q="。
func NormalizeEndpoint(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// 先去方法前缀(完整 URL 前也常带 "GET http://..." 形态)
	s = methodPrefixRe.ReplaceAllString(s, "")
	// 完整 URL → 只取 path(url.Parse 已剥离 query)
	if i := strings.Index(s, "://"); i >= 0 {
		if u, err := url.Parse(s); err == nil && u.Path != "" {
			s = u.Path
		} else if err == nil {
			s = "/" // 无路径的完整 URL(http://host) → 根路径
		}
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	if dec, err := url.PathUnescape(s); err == nil {
		s = dec
	}
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return "/"
	}
	return s
}

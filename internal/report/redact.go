package report

import (
	"regexp"

	"scopeforge/internal/reasonix/secrets"
)

// ------------------------------------------------------------------ 05 §3 脱敏

// PII 打码正则:邮箱 / 中国大陆手机号 / 18 位身份证(05 §3 个人数据)。
var (
	piiEmailRe  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	piiPhoneRe  = regexp.MustCompile(`(?:\+?86[\s\-]?)?1[3-9]\d{9}`)
	piiIDCardRe = regexp.MustCompile(`\b\d{17}[\dXx]\b`)
)

// Redact 报告脱敏(05 §3 强制步骤):凭据 + 内网 IP + 个人数据(邮箱/手机/身份证)。
func Redact(md string) string {
	md = secrets.RedactCredentials(md)
	// 内网 IP 掩码(10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16)
	re := regexp.MustCompile(`\b(10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|169\.254\.\d{1,3}\.\d{1,3})\b`)
	md = re.ReplaceAllString(md, "[internal-ip]")
	// 个人数据(05 §3 红线:报告生成前扫描打码)
	md = piiEmailRe.ReplaceAllString(md, "[email]")
	md = piiPhoneRe.ReplaceAllString(md, "[phone]")
	md = piiIDCardRe.ReplaceAllString(md, "[id-card]")
	return md
}

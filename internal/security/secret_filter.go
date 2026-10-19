package security

import "regexp"

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)(\b(?:api[_-]?(?:key|token)|access[_-]?token|auth[_-]?token|refresh[_-]?token|token|password|secret|credential)\b["']?\s*[:=]\s*)("[^"\r\n]*"|'[^'\r\n]*'|[^\s;,]+)`),
	regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16})\b`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{10,}`),
	regexp.MustCompile(`(?i)(\bAuthorization\s*:\s*)(?:Bearer|Basic)\s+\S+`),
}

// RedactSecrets is a best-effort output filter; sandboxing remains the security boundary.
func RedactSecrets(value string) string {
	value = secretPatterns[0].ReplaceAllString(value, "[REDACTED PRIVATE KEY]")
	value = secretPatterns[4].ReplaceAllString(value, `${1}[REDACTED]`)
	value = secretPatterns[1].ReplaceAllString(value, `${1}[REDACTED]`)
	value = secretPatterns[3].ReplaceAllString(value, "Bearer [REDACTED]")
	return secretPatterns[2].ReplaceAllString(value, "[REDACTED TOKEN]")
}

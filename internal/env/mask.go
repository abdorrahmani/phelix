package env

import (
	"regexp"
	"strings"
)

// SensitiveKeys defines keys that should be masked in logs
var SensitiveKeys = []string{
	"SECRET", "KEY", "TOKEN", "PASSWORD",
	"APIKEY", "API_KEY", "PRIVATE_KEY",
	"AUTH", "CREDENTIAL", "CREDENTIALS",
	"CERT", "CERTIFICATE", "KEY_SECRET",
	"AWS_SECRET", "DATABASE_PASSWORD",
}

// MaskSensitiveData masks sensitive key values in logs
func MaskSensitiveData(text string) string {
	for _, key := range SensitiveKeys {
		// Match KEY=value, KEY: value, KEY = value patterns
		patterns := []string{
			regexp.QuoteMeta(key) + "=([^\\s,;}]+)",
			regexp.QuoteMeta(key) + ":\\s*([^\\s,;}]+)",
			regexp.QuoteMeta(key) + "\\s*=\\s*([^\\s,;}]+)",
		}

		for _, pattern := range patterns {
			re := regexp.MustCompile("(?i)" + pattern)
			text = re.ReplaceAllString(text, key+"=***REDACTED***")
		}
	}
	return text
}

// IsSensitiveKey checks if a key is sensitive
func IsSensitiveKey(key string) bool {
	key = strings.ToUpper(key)
	for _, sensitive := range SensitiveKeys {
		if strings.Contains(key, sensitive) {
			return true
		}
	}
	return false
}

// MaskValue returns a masked version of the value if key is sensitive
func MaskValue(key, value string) string {
	if IsSensitiveKey(key) {
		return "***REDACTED***"
	}
	return value
}

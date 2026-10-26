package zharp

import (
	"os"
	"regexp"
	"runtime/debug"
	"strings"
)

var trueValue = regexp.MustCompile(`(?i)^(true|1|yes|on)$`)

// envValue is an environment variable; "" when unset or blank.
func envValue(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// pick is the first non-blank value — explicit option, then each fallback in turn.
func pick(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func isTrue(value string) bool { return trueValue.MatchString(value) }

// moduleName is the app's own name: the last part of the main module's path, the name a service
// gets when none is configured. "" when the binary carries no build info.
func moduleName() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Path == "" || info.Main.Path == "command-line-arguments" {
		return ""
	}
	path := strings.TrimSuffix(info.Main.Path, "/")
	// A major-version suffix isn't the name: github.com/acme/api/v2 → api.
	parts := strings.Split(path, "/")
	name := parts[len(parts)-1]
	if len(parts) > 1 && len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		name = parts[len(parts)-2]
	}
	return name
}

func normalizeEndpoint(endpoint string) string {
	if strings.TrimSpace(endpoint) == "" {
		return "https://api.zharp.io/api/v1"
	}
	return strings.TrimRight(strings.TrimSpace(endpoint), "/")
}

// announce is startup output, on stderr. One prefix, so it can be searched for.
func announce(message string) {
	_, _ = os.Stderr.WriteString("[Zharp] " + message + "\n")
}

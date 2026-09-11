package llm

import "strings"

// ChildEnvironment builds the default-deny environment used for child
// processes that execute models. Only process basics, proxy/certificate
// settings, and the explicitly selected credential are retained.
func ChildEnvironment(base []string, credentialKey, credentialValue string) []string {
	allowed := map[string]struct{}{
		"COLORTERM": {}, "HOME": {}, "HTTPS_PROXY": {}, "HTTP_PROXY": {},
		"LANG": {}, "LC_ALL": {}, "LOGNAME": {}, "NO_PROXY": {}, "PATH": {},
		"CURL_CA_BUNDLE": {}, "GIT_SSL_CAINFO": {}, "NODE_EXTRA_CA_CERTS": {},
		"SHELL": {}, "SSL_CERT_DIR": {}, "SSL_CERT_FILE": {}, "TERM": {},
		"TERM_PROGRAM": {}, "TMPDIR": {}, "TZ": {}, "USER": {},
		"XDG_CACHE_HOME": {}, "XDG_CONFIG_HOME": {}, "XDG_DATA_HOME": {},
		"http_proxy": {}, "https_proxy": {}, "no_proxy": {},
	}
	if credentialKey == "ANTHROPIC_API_KEY" {
		// Preserve the Claude CLI's established token and gateway auth modes, but
		// only for Claude children. OpenRouter/Codex must never receive them.
		allowed["CLAUDE_CODE_OAUTH_TOKEN"] = struct{}{}
		allowed["ANTHROPIC_AUTH_TOKEN"] = struct{}{}
		allowed["ANTHROPIC_BASE_URL"] = struct{}{}
	}
	environment := make([]string, 0, len(base)+1)
	seen := make(map[string]struct{}, len(allowed))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == credentialKey {
			continue
		}
		if _, keep := allowed[key]; !keep {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		environment = append(environment, entry)
	}
	if credentialValue != "" {
		environment = append(environment, credentialKey+"="+credentialValue)
	}
	return environment
}

// Package tickets gives the review agent the intent recorded in the Jira
// tickets a PR references directly (title, body, branch name): key
// extraction, a minimal Jira fetcher, and the prompt section that carries
// the PR text plus ticket summaries and comments.
package tickets

import (
	"regexp"
	"strings"
)

const maxKeys = 3

var keyPattern = regexp.MustCompile(`[A-Z][A-Z0-9]{1,9}-[0-9]{1,6}`)

// Upper-case token-dash-number shapes that show up in PR text without being
// ticket keys. Only consulted when no project allowlist is configured.
var nonTicketProjects = map[string]bool{
	"UTF": true, "ISO": true, "RFC": true, "SHA": true, "MD": true, "HTTP": true,
	"TLS": true, "SSL": true, "IPV": true, "AES": true, "RSA": true, "CVE": true,
}

// ExtractKeys returns the Jira keys referenced in texts, case-sensitive,
// deduplicated in order of first appearance and capped at three. A non-empty
// projectKeys restricts matches to those projects; otherwise any project is
// accepted except a blocklist of common non-ticket tokens.
func ExtractKeys(projectKeys []string, texts ...string) []string {
	allowed := make(map[string]bool, len(projectKeys))
	for _, p := range projectKeys {
		if p = strings.TrimSpace(p); p != "" {
			allowed[p] = true
		}
	}
	var keys []string
	seen := map[string]bool{}
	for _, text := range texts {
		for _, loc := range keyPattern.FindAllStringIndex(text, -1) {
			if !tokenBounded(text, loc[0], loc[1]) {
				continue
			}
			key := text[loc[0]:loc[1]]
			project := key[:strings.IndexByte(key, '-')]
			if len(allowed) > 0 {
				if !allowed[project] {
					continue
				}
			} else if nonTicketProjects[project] {
				continue
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			keys = append(keys, key)
			if len(keys) == maxKeys {
				return keys
			}
		}
	}
	return keys
}

func tokenBounded(text string, start, end int) bool {
	return (start == 0 || !isAlnum(text[start-1])) && (end == len(text) || !isAlnum(text[end]))
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

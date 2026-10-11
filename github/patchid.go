package github

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

// PatchID is a stable id of a unified diff: a hash of its lines with hunk
// line numbers, index lines and trailing whitespace removed, so a merge from
// the base branch, a rebase or an amended message leaves it unchanged while
// any change to the diffed lines, indentation included, moves it. An empty
// diff has no id.
func PatchID(diff string) string {
	if strings.TrimSpace(diff) == "" {
		return ""
	}
	h := sha256.New()
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "index "), strings.HasPrefix(line, "similarity index "), strings.HasPrefix(line, "dissimilarity index "):
			continue
		case strings.HasPrefix(line, "@@"):
			line = "@@"
		}
		line = strings.TrimRightFunc(line, unicode.IsSpace)
		if line == "" {
			continue
		}
		h.Write([]byte(line))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:40]
}

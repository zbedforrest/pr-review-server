package payload

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// leadingProvenanceNoteRe matches the whole italic note the merge layer
// prepends to re-admitted findings. It carries a source SHA for carried
// findings and would otherwise dominate the hashed prefix, so identity is
// computed from the text after it.
var leadingProvenanceNoteRe = regexp.MustCompile(`^\s*_\[[^\]]*\]_\s*`)

// Fingerprint shape: <file>:<line/10>:<hex(sha256(prefix))[:12]> where prefix
// is the first 120 runes of the comment lower-cased with whitespace runs
// collapsed. Line numbers drift across pushes and comment tails vary between
// runs, so the line is bucketed and only a normalized prefix is hashed.
// Mirrored in benchmark/harvest_outcomes.py; keep the two in sync.
const (
	fingerprintLineBucket    = 10
	fingerprintCommentPrefix = 120
	fingerprintHashHexLen    = 12
)

// Fingerprint is the stable identity of a finding across re-reviews of the
// same PR, shared by the sidecar, the publisher, and finding outcomes.
func Fingerprint(file string, line int, comment string) string {
	comment = leadingProvenanceNoteRe.ReplaceAllString(comment, "")
	norm := strings.Join(strings.Fields(strings.ToLower(comment)), " ")
	if r := []rune(norm); len(r) > fingerprintCommentPrefix {
		norm = string(r[:fingerprintCommentPrefix])
	}
	sum := sha256.Sum256([]byte(norm))
	bucket := 0
	if line > 0 {
		bucket = line / fingerprintLineBucket
	}
	return fmt.Sprintf("%s:%d:%s", file, bucket, hex.EncodeToString(sum[:])[:fingerprintHashHexLen])
}

// FingerprintFile recovers the file a fingerprint cites. Line bucket and hash
// never contain a colon, so the file is everything before the last two.
func FingerprintFile(fp string) string {
	i := strings.LastIndex(fp, ":")
	if i < 0 {
		return fp
	}
	j := strings.LastIndex(fp[:i], ":")
	if j < 0 {
		return fp
	}
	return fp[:j]
}

package payload

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

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

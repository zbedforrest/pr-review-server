package tickets

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractKeysFindsKeysAcrossTextsInOrderOfFirstAppearance(t *testing.T) {
	keys := ExtractKeys(nil, "XO-370 tighten retries", "Also touches AB1-12 and refs XO-370 again.", "feature/zz-9-thing")
	assert.Equal(t, []string{"XO-370", "AB1-12"}, keys)
}

func TestExtractKeysIsCaseSensitive(t *testing.T) {
	assert.Empty(t, ExtractKeys(nil, "xo-370 lowercase should not match"))
}

func TestExtractKeysCapsAtThree(t *testing.T) {
	keys := ExtractKeys(nil, "XO-1 XO-2 XO-3 XO-4 XO-5")
	assert.Equal(t, []string{"XO-1", "XO-2", "XO-3"}, keys)
}

func TestExtractKeysHonorsProjectAllowlist(t *testing.T) {
	keys := ExtractKeys([]string{"XO"}, "AB-1 XO-2 XOX-3 XO-4")
	assert.Equal(t, []string{"XO-2", "XO-4"}, keys)
}

func TestExtractKeysWithoutAllowlistSkipsCommonNonTicketTokens(t *testing.T) {
	text := "UTF-8 ISO-8601 RFC-2119 SHA-256 MD-5 HTTP-2 TLS-13 SSL-3 IPV-6 AES-256 RSA-2048 CVE-2024 XO-9"
	assert.Equal(t, []string{"XO-9"}, ExtractKeys(nil, text))
}

func TestExtractKeysWithoutAllowlistSkipsLongNumbers(t *testing.T) {
	assert.Empty(t, ExtractKeys(nil, "CB-1234567 has too many digits"))
	assert.Equal(t, []string{"CB-123456"}, ExtractKeys(nil, "CB-123456 is fine"))
}

func TestExtractKeysRequiresTokenBoundaries(t *testing.T) {
	assert.Empty(t, ExtractKeys(nil, "prefixXO-12", "XO-12suffix"))
	assert.Equal(t, []string{"XO-12"}, ExtractKeys(nil, "[XO-12] wrapped", "(XO-12)"))
}

func TestExtractKeysHandlesEmptyInput(t *testing.T) {
	assert.Empty(t, ExtractKeys(nil))
	assert.Empty(t, ExtractKeys(nil, "", ""))
}

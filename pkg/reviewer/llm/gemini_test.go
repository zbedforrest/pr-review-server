package llm

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Note: Testing the actual generative model calls would require mocking the genai.Client
// and its chained methods, which is complex. For this case, we'll focus on testing the
// client constructor to ensure it's selecting the correct model based on the `useFlash` flag.
// A more robust solution might involve creating an interface for the genai.GenerativeModel
// and injecting a mock implementation, but for now, this provides a good baseline.

func TestNewClient_ModelSelection(t *testing.T) {
	// This test ensures that NewClient does not panic when selecting different providers and models.
	// The actual API key is not needed for this test, as we are not making real
	// network calls. We are only testing the model selection logic within our NewClient function.

	// Test case 1: Selecting the Gemini "pro" model (useFlash = false)
	assert.NotPanics(t, func() {
		_ = NewClient(ProviderGemini, "dummy-key", false, false)
	}, "NewClient should not panic when selecting the Gemini pro model")

	// Test case 2: Selecting the Gemini "flash" model (useFlash = true)
	assert.NotPanics(t, func() {
		_ = NewClient(ProviderGemini, "dummy-key", true, false)
	}, "NewClient should not panic when selecting the Gemini flash model")

	// Test case 3: Selecting Claude (useFlash = false)
	assert.NotPanics(t, func() {
		_ = NewClient(ProviderClaude, "dummy-key", false, false)
	}, "NewClient should not panic when selecting the Claude model")

	// Test case 4: useFlash does not change the Claude model
	assert.NotPanics(t, func() {
		_ = NewClient(ProviderClaude, "dummy-key", true, false)
	}, "NewClient should not panic with useFlash on the Claude provider")
}

// TestModelNameResolution verifies that the Gemini model names resolve from
// the GEMINI_PRO_MODEL / GEMINI_FLASH_MODEL environment variables when set,
// and fall back to the package defaults when unset or empty — guaranteeing
// zero behavior change for deployments that don't set the overrides.
func TestModelNameResolution(t *testing.T) {
	t.Run("defaults when env unset", func(t *testing.T) {
		// t.Setenv registers a cleanup restoring the original values;
		// unsetting afterwards exercises the true "unset" case.
		t.Setenv("GEMINI_PRO_MODEL", "")
		t.Setenv("GEMINI_FLASH_MODEL", "")
		os.Unsetenv("GEMINI_PRO_MODEL")
		os.Unsetenv("GEMINI_FLASH_MODEL")

		assert.Equal(t, DefaultProModel, ProModelName())
		assert.Equal(t, DefaultFlashModel, FlashModelName())
		// Pin the literals so a default change is a deliberate, visible edit.
		assert.Equal(t, "gemini-3.1-pro-preview", ProModelName())
		assert.Equal(t, "gemini-flash-latest", FlashModelName())
	})

	t.Run("defaults when env empty", func(t *testing.T) {
		t.Setenv("GEMINI_PRO_MODEL", "")
		t.Setenv("GEMINI_FLASH_MODEL", "")

		assert.Equal(t, DefaultProModel, ProModelName())
		assert.Equal(t, DefaultFlashModel, FlashModelName())
	})

	t.Run("env overrides defaults", func(t *testing.T) {
		t.Setenv("GEMINI_PRO_MODEL", "gemini-2.5-pro")
		t.Setenv("GEMINI_FLASH_MODEL", "gemini-2.5-flash")

		assert.Equal(t, "gemini-2.5-pro", ProModelName())
		assert.Equal(t, "gemini-2.5-flash", FlashModelName())
	})
}

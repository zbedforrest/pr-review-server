package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pr-review-server/pkg/reviewer/html"
)

func TestGenerateHTMLReportContent_LeavesThePromptOffThePage(t *testing.T) {
	result := &ReviewResult{PRTitle: "t", Prompt: "distinctivePromptText"}
	report := GenerateHTMLReportContent(result, 7, "acme", "example", "abcdef1", "model-x", html.PublishView{})
	require.NotEmpty(t, report)
	assert.NotContains(t, string(report), "distinctivePromptText")
}

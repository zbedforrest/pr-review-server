package publisher

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"pr-review-server/pkg/reviewer/payload"
)

func TestRoundDecisionsMapsSurvivorsBackToTheirArrivalIDs(t *testing.T) {
	r := Round{
		Findings:       []payload.Finding{{ID: "ledger-a"}, {ID: "b"}},
		InlineComments: map[string]int64{"ledger-a": 11, "gone": 12},
	}
	held := map[string]payload.Finding{"c": {ID: "c"}}
	d := roundDecisions([]string{"a", "b", "c", "settled"}, map[string]string{"ledger-a": "a"}, r, held)
	assert.Equal(t, map[string]bool{"settled": true}, d.Dropped)
	assert.Equal(t, map[string]bool{"a": true}, d.Inline)
}

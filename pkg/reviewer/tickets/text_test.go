package tickets

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHTMLToTextStripsTagsDecodesEntitiesAndCollapsesWhitespace(t *testing.T) {
	in := "<p>Keep   the &lt;retry&gt; &amp; backoff.</p>\n\n<p>Second&nbsp;para</p><ul><li>one</li><li>two</li></ul><br/>tail"
	got := htmlToText(in)
	assert.Equal(t, "Keep the <retry> & backoff.\nSecond para\none\ntwo\ntail", got)
}

func TestHTMLToTextDropsScriptAndStyleBodies(t *testing.T) {
	assert.Equal(t, "visible", htmlToText("<style>p{}</style><script>x()</script>visible"))
}

func TestADFToTextConcatenatesTextNodesWithBlockBoundaries(t *testing.T) {
	doc := []byte(`{
	  "type":"doc","version":1,"content":[
	    {"type":"paragraph","content":[{"type":"text","text":"We decided "},{"type":"text","text":"to keep retries.","marks":[{"type":"strong"}]}]},
	    {"type":"bulletList","content":[
	      {"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"first"}]}]},
	      {"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"second"}]}]}
	    ]},
	    {"type":"paragraph","content":[{"type":"hardBreak"},{"type":"text","text":"after"},{"type":"mention","attrs":{"text":"@alice"}}]}
	  ]}`)
	assert.Equal(t, "We decided to keep retries.\nfirst\nsecond\nafter @alice", adfToText(doc))
}

func TestADFToTextToleratesGarbage(t *testing.T) {
	assert.Equal(t, "", adfToText(nil))
	assert.Equal(t, "", adfToText([]byte(`"just a string"`)))
	assert.Equal(t, "", adfToText([]byte(`{not json`)))
}

func TestTruncateRunesAddsMarkerOnlyWhenCut(t *testing.T) {
	assert.Equal(t, "héllo", truncateRunes("héllo", 5))
	assert.Equal(t, "hé...", truncateRunes("héllo!", 5))
}

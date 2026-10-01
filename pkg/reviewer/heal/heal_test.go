package heal

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

const two = `{"findings":[` +
	`{"id":"A-1","file_path":"app/views.py","line_number":40,"importance":"CRITICAL","comment_body":"Anonymous users crash because request.user.id is None."},` +
	`{"id":"A-2","file_path":"app/forms.py","line_number":12,"importance":"LOW","comment_body":"The \"should not appear again\" case has no test."}]}`

func decoded(t *testing.T, r Result) []map[string]any {
	t.Helper()
	var doc struct {
		Findings []map[string]any `json:"findings"`
	}
	if err := json.Unmarshal(r.JSON, &doc); err != nil {
		t.Fatalf("healed JSON does not parse: %v", err)
	}
	return doc.Findings
}

func TestHealRecoversEachObservedFailureModeWithoutChangingContent(t *testing.T) {
	for name, tc := range map[string]struct {
		raw    string
		method string
	}{
		"clean with trailing prose": {two + "\n\nThat is all.", MethodClean},
		"array closed with a brace": {two[:len(two)-2] + "}}", MethodRebalanced},
		"truncated before closers":  {two[:len(two)-3], MethodRebalanced},
		"trailing comma before a closer": {
			`{"findings":[{"id":"A-1","file_path":"app/views.py","line_number":40,"importance":"CRITICAL","comment_body":"Anonymous users crash because request.user.id is None.",},` +
				`{"id":"A-2","file_path":"app/forms.py","line_number":12,"importance":"LOW","comment_body":"The \"should not appear again\" case has no test."}]}`,
			MethodRequoted,
		},
		"single quotes around keys": {
			`{"findings":[{"id":"A-1","file_path":"app/views.py","line_number":40,"comment_body":"Anonymous users crash because request.user.id is None.','importance':'CRITICAL'},` +
				`{"id":"A-2","file_path":"app/forms.py","line_number":12,"importance":"LOW","comment_body":"The \"should not appear again\" case has no test."}]}`,
			MethodRequoted,
		},
		"garbage tail after the last value": {two[:len(two)-3] + `","}}]}"}}]}`, MethodTrimmed},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := Heal(tc.raw)
			if err != nil {
				t.Fatalf("Heal: %v", err)
			}
			if res.Method != tc.method {
				t.Errorf("method = %s, want %s", res.Method, tc.method)
			}
			fs := decoded(t, res)
			if len(fs) != 2 {
				t.Fatalf("got %d findings, want 2: %s", len(fs), res.JSON)
			}
			if fs[0]["comment_body"] != "Anonymous users crash because request.user.id is None." ||
				fs[1]["comment_body"] != `The "should not appear again" case has no test.` {
				t.Errorf("content changed: %s", res.JSON)
			}
		})
	}
}

func TestHealJoinsAnObjectTheModelClosedEarly(t *testing.T) {
	raw := `{"findings":[{"id":"A-1","file_path":"app/views.py","line_number":40,"comment_body":"Anonymous users crash on the stats page."},` +
		`{"file_path":"SUMMARY","line_number":0}, "id":"SUMMARY","comment_body":"Verdict: request changes before merging."}]}`
	res, err := Heal(raw)
	if err != nil {
		t.Fatalf("Heal: %v", err)
	}
	fs := decoded(t, res)
	if len(fs) != 2 || fs[1]["file_path"] != "SUMMARY" || fs[1]["comment_body"] != "Verdict: request changes before merging." {
		t.Fatalf("summary fragment not rejoined: %s", res.JSON)
	}
}

func TestHealKeepsASummaryBlockBesideTheFindings(t *testing.T) {
	raw := two[:len(two)-1] + `,"summary":{"verdict":"request_changes","upshot":"Fix the anonymous crash first."}` + ",}"
	res, err := Heal(raw)
	if err != nil {
		t.Fatalf("Heal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(res.JSON, &doc); err != nil {
		t.Fatal(err)
	}
	if sum, _ := doc["summary"].(map[string]any); sum["upshot"] != "Fix the anonymous crash first." {
		t.Fatalf("summary block lost: %s", res.JSON)
	}
}

func TestHealReportsProseAnswersAsNoJSON(t *testing.T) {
	if _, err := Heal("Verdict: approve.\n\nThe change is correct and safe to merge."); !errors.Is(err, ErrNoJSON) {
		t.Fatalf("err = %v, want ErrNoJSON", err)
	}
}

func TestAcceptRejectsARepairThatChangedAWord(t *testing.T) {
	raw := `{"findings":[{"file_path":"app/views.py","comment_body":"Anonymous users crash on the stats page."}]}`
	reworded := `{"findings":[{"file_path":"app/views.py","comment_body":"Signed-out users crash on the stats page."}]}`
	if _, ok := accept(reworded, MethodRepaired, keyCounts(raw), normalizeRaw(raw)); ok {
		t.Fatal("a reworded finding passed the verbatim check")
	}
}

func TestAcceptRejectsARepairThatDroppedAField(t *testing.T) {
	raw := `{"findings":[{"file_path":"app/views.py","importance":"CRITICAL","comment_body":"Anonymous users crash on the stats page."}]}`
	noSeverity := `{"findings":[{"file_path":"app/views.py","comment_body":"Anonymous users crash on the stats page."}]}`
	if _, ok := accept(noSeverity, MethodRepaired, keyCounts(raw), normalizeRaw(raw)); ok {
		t.Fatal("a repair that lost the severity field passed the key check")
	}
}

func TestAcceptRejectsARepairThatDroppedAFinding(t *testing.T) {
	onlyFirst := `{"findings":[{"file_path":"app/views.py","comment_body":"Anonymous users crash because request.user.id is None."}]}`
	if _, ok := accept(onlyFirst, MethodRepaired, keyCounts(two), normalizeRaw(two)); ok {
		t.Fatal("a repair that lost a finding passed the completeness check")
	}
}

func TestRebalanceLeavesStringsAlone(t *testing.T) {
	in := `{"a":"brackets } ] { [ inside","b":[1,2}`
	want := `{"a":"brackets } ] { [ inside","b":[1,2]}`
	if got := Rebalance(in); got != want {
		t.Fatalf("Rebalance = %s, want %s", got, want)
	}
	var v any
	if err := json.Unmarshal([]byte(Rebalance(`{"a":"open string`)), &v); err != nil || !reflect.DeepEqual(v, map[string]any{"a": "open string"}) {
		t.Fatalf("open string not closed: %v %v", v, err)
	}
}

func TestHealObjectRepairsStructureWithoutInventingContent(t *testing.T) {
	out, _, err := HealObject("Here you go:\n{\"summary\": \"ok\", \"concerns\": [{\"id\": \"c1\"}]")
	if err != nil || string(out) != `{"concerns":[{"id":"c1"}],"summary":"ok"}` {
		t.Fatalf("truncated object not repaired: %s %v", out, err)
	}
	if _, _, err := HealObject("no json here"); err == nil {
		t.Fatal("text without an object must not heal")
	}
}

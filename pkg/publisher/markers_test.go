package publisher

import "testing"

func TestParseHunks_ReadsBothSidesOfEveryHeader(t *testing.T) {
	patch := "@@ -3,7 +3,6 @@ import x\n-a\n b\n@@ -113,9 +112,7 @@\n c\n@@ -0,0 +1 @@\n+new\n@@ -40 +41,2 @@\n-x\n+y\n+z\n"
	got := ParseHunks(patch)
	want := []Hunk{{3, 7, 3, 6}, {113, 9, 112, 7}, {0, 0, 1, 1}, {40, 1, 41, 2}}
	if len(got) != len(want) {
		t.Fatalf("hunks = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hunk %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if ParseHunks("") != nil {
		t.Fatal("an empty patch has no hunks")
	}
}

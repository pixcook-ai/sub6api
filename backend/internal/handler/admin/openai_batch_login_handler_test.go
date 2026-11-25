package admin

import "testing"

func TestParseBatchLoginText(t *testing.T) {
	entries, ok := parseBatchLoginText("a@b.co----pw1----JBSWY3DP\r\n\r\n c@d.co ---- pw2 ----ABCD\n")
	if !ok || len(entries) != 2 {
		t.Fatalf("ok=%v entries=%+v", ok, entries)
	}
	if entries[0].Email != "a@b.co" || entries[0].Password != "pw1" || entries[0].TOTPSecret != "JBSWY3DP" {
		t.Fatalf("entry 0: %+v", entries[0])
	}
	if entries[1].Email != "c@d.co" || entries[1].Password != "pw2" {
		t.Fatalf("entry 1: %+v", entries[1])
	}
	for _, bad := range []string{"a@b.co----pw", "a@b.co----pw----ABCD----extra", "a@b.co,pw,ABCD"} {
		if _, ok := parseBatchLoginText(bad); ok {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

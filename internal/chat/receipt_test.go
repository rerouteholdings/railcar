package chat

import "testing"

// TestReceiptTimeWrong checks when a member warns about a receipt's time
// (§11.8).
func TestReceiptTimeWrong(t *testing.T) {
	now := int64(1_800_000_000)
	for _, c := range []struct {
		prev, ts int64
		warn     bool
	}{
		{0, now, false},
		{now - 10, now, false},
		{now, now - 1, true},
		{0, now + 3601, true},
		{0, now + 3600, false},
	} {
		if got := receiptTimeWrong(c.prev, c.ts, now) != ""; got != c.warn {
			t.Errorf("prev %d, ts %d: warn %v", c.prev, c.ts, got)
		}
	}
}

// TestCheckUsed checks the used list's rules, which the members group now
// follows too (§11.8, §16.6).
func TestCheckUsed(t *testing.T) {
	old := &GroupExt{Used: map[string]int64{"a": 100, "b": 300}}
	appr := &AddApproval{Request: "c", Exp: 500}
	ok := &GroupExt{Used: map[string]int64{"b": 300, "c": 500}}
	if err := checkUsed(old, ok, 200, appr); err != nil {
		t.Errorf("dropping an expired entry and adding the used approval: %v", err)
	}
	for name, next := range map[string]*GroupExt{
		"drops one not expired": {Used: map[string]int64{"a": 100, "c": 500}},
		"changes one":           {Used: map[string]int64{"a": 100, "b": 301, "c": 500}},
		"adds another":          {Used: map[string]int64{"a": 100, "b": 300, "d": 1}},
	} {
		if err := checkUsed(old, next, 200, appr); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := checkUsed(old, &GroupExt{Used: map[string]int64{"a": 100, "b": 300, "c": 500}}, 200, nil); err == nil {
		t.Error("an entry added without an approval")
	}
}

package fixturecost

import "testing"

func TestOperationBudgetRejectsExcessAndInvalidLimits(t *testing.T) {
	for _, limits := range []map[string]int64{{"commits": 1}, {"commits": -1}} {
		if err := exceeded(map[string]int64{"commits": 2}, limits); err == nil {
			t.Fatal("accepted excessive work or invalid budget")
		}
	}
	if err := exceeded(nil, map[string]int64{"commits": 0}); err == nil {
		t.Fatal("accepted missing measurement")
	}
	if err := exceeded(map[string]int64{"commits": 2}, map[string]int64{"commits": 2}); err != nil {
		t.Fatal(err)
	}
}

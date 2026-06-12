package search

import "testing"

func TestSubsequenceMatch(t *testing.T) {
	cases := []struct {
		q    string
		text string
		ok   bool
	}{
		{"doc", "Documents", true},
		{"dcm", "Documents", true},
		{"xyz", "Documents", false},
		{"", "anything", true},
	}
	for _, c := range cases {
		got := SubsequenceMatch(c.q, c.text)
		if got != c.ok {
			t.Fatalf("query %q text %q expected %v got %v", c.q, c.text, c.ok, got)
		}
	}
}

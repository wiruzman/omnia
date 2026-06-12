package search

import "strings"

func SubsequenceMatch(query, text string) bool {
	q := []rune(strings.ToLower(strings.TrimSpace(query)))
	t := []rune(strings.ToLower(text))
	if len(q) == 0 {
		return true
	}
	j := 0
	for _, ch := range t {
		if ch == q[j] {
			j++
			if j == len(q) {
				return true
			}
		}
	}
	return false
}

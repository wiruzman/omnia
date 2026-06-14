package store

import (
	"strings"
	"unicode/utf8"
)

const (
	minContainsRunes      = 3
	minBroadPathRunes     = 4
	shortPrefixEnoughRows = 200
)

type queryPlan struct {
	query            string
	terms            []string
	pathLike         bool
	absolutePathLike bool
}

func planQuery(query string) queryPlan {
	qLower := strings.ToLower(strings.TrimSpace(query))
	return queryPlan{
		query:            qLower,
		terms:            strings.Fields(qLower),
		pathLike:         strings.Contains(qLower, "/"),
		absolutePathLike: strings.HasPrefix(qLower, "/"),
	}
}

func (p queryPlan) allowNameContains() bool {
	return !p.pathLike && runeLen(p.query) >= minContainsRunes
}

func (p queryPlan) allowPathContains(currentMatches int) bool {
	if p.pathLike {
		return true
	}
	if currentMatches > 0 {
		return false
	}
	return runeLen(p.query) >= minBroadPathRunes
}

func (p queryPlan) allowAllTermContains() bool {
	if len(p.terms) < 2 {
		return false
	}
	for _, term := range p.terms {
		if runeLen(term) < minContainsRunes {
			return false
		}
	}
	return true
}

func (p queryPlan) shouldStopAfterPrefix(entries, limit int) bool {
	if p.pathLike || runeLen(p.query) >= minBroadPathRunes {
		return false
	}
	return entries >= minInt(limit, shortPrefixEnoughRows)
}

func runeLen(value string) int {
	return utf8.RuneCountInString(value)
}

// Package classify assigns a task category from its description using a
// deterministic keyword match.
package classify

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Default is the category used when no keyword matches.
const Default = "engineering"

// Names lists the valid categories.
var Names = []string{"engineering", "marketing", "finance", "sales", "compliance"}

// categories is ordered by tie-break priority, highest first.
var categories = []struct {
	name     string
	keywords []string
}{
	{"compliance", []string{"gdpr", "soc2", "sox", "hipaa", "pci", "compliance", "audit", "policy", "regulat", "legal", "privacy", "kyc", "aml", "consent", "retention"}},
	{"finance", []string{"budget", "invoic", "expense", "payroll", "forecast", "revenue", "cost", "accounting", "tax", "reimburs", "financ", "quarterly", "p&l", "pnl"}},
	{"sales", []string{"lead", "deal", "prospect", "quota", "crm", "demo", "pricing", "proposal", "customer", "renewal", "upsell", "sales"}},
	{"marketing", []string{"campaign", "brand", "social", "seo", "newsletter", "blog", "launch", "ad", "ads", "advertis", "content", "webinar", "promo", "press", "market"}},
	{"engineering", []string{"bug", "fix", "api", "code", "deploy", "build", "refactor", "test", "database", "db", "backend", "frontend", "server", "library", "auth", "login", "migrat", "endpoint", "implement", "infra", "pipeline", "ci", "release", "sdk", "perf", "latency", "outage"}},
}

// Classify returns the best-scoring category for description and whether
// it fell back to Default because nothing matched. Each word scores at most
// one hit per category; ties go to the earlier category in the priority
// order compliance, finance, sales, marketing, engineering.
func Classify(description string) (category string, defaulted bool) {
	words := Words(description)
	best, bestScore := Default, 0
	for _, c := range categories {
		score := 0
		for _, w := range words {
			for _, kw := range c.keywords {
				if matches(w, kw) {
					score++
					break
				}
			}
		}
		if score > bestScore {
			best, bestScore = c.name, score
		}
	}
	return best, bestScore == 0
}

// matches reports whether word hits keyword: keywords of up to three
// characters must equal the word, longer ones match as a prefix.
func matches(word, keyword string) bool {
	if utf8.RuneCountInString(keyword) <= 3 {
		return word == keyword
	}
	return strings.HasPrefix(word, keyword)
}

// Words lowercases s and splits it into runs of letters and digits. An '&'
// between two such characters stays inside the word so "P&L" is one word.
func Words(s string) []string {
	runes := []rune(strings.ToLower(s))
	isWordRune := func(i int) bool {
		return i >= 0 && i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i]))
	}
	var words []string
	var cur []rune
	for i, r := range runes {
		switch {
		case isWordRune(i):
			cur = append(cur, r)
		case r == '&' && isWordRune(i-1) && isWordRune(i+1):
			cur = append(cur, r)
		default:
			if len(cur) > 0 {
				words = append(words, string(cur))
				cur = nil
			}
		}
	}
	if len(cur) > 0 {
		words = append(words, string(cur))
	}
	return words
}

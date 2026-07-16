package search

import (
	"strconv"
	"strings"
)

// Query-time synonym expansion, matching the hosted service. Synonyms are INSTANCE
// config (admin PUT), never part of the request.
// Expansion is additive — the original term always stays one branch of the OR — and it
// happens at the term-compilation leaf, so `NOT laptop` naturally excludes the synonyms
// too (the negation wraps the expanded set).
//
// Emulator fidelity note: single-word dictionary keys expand (multi-word alternatives are
// emitted as phrases); multi-word KEYS ("laptop computer" as a source) are not matched
// against word runs here — the hosted engine does that. Numeral synonyms (digit <->
// roman <-> words) are fully supported.

// SynonymConfig mirrors the instance-config `synonyms` object.
type SynonymConfig struct {
	Equivalents [][]string
	OneWay      []OneWayRule
	NumberRoman string // off | oneway | twoway
	NumberWords string
}

type OneWayRule struct {
	From string
	To   []string
}

func (c *SynonymConfig) empty() bool {
	return c == nil || (len(c.Equivalents) == 0 && len(c.OneWay) == 0 &&
		(c.NumberRoman == "" || c.NumberRoman == "off") && (c.NumberWords == "" || c.NumberWords == "off"))
}

// parseSynonyms tolerantly reads the stored config value (a decoded-JSON map). A
// malformed blob yields nil — bad config must never take the query path down.
func parseSynonyms(v any) *SynonymConfig {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	c := &SynonymConfig{}
	if eq, ok := m["equivalents"].([]any); ok {
		for _, set := range eq {
			arr, ok := set.([]any)
			if !ok {
				continue
			}
			var terms []string
			for _, t := range arr {
				if s, ok := t.(string); ok {
					if n := normPhrase(s); n != "" {
						terms = append(terms, n)
					}
				}
			}
			if len(terms) >= 2 {
				c.Equivalents = append(c.Equivalents, terms)
			}
		}
	}
	if ow, ok := m["oneWay"].([]any); ok {
		for _, r := range ow {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			from, _ := rm["from"].(string)
			from = normPhrase(from)
			var to []string
			if ta, ok := rm["to"].([]any); ok {
				for _, t := range ta {
					if s, ok := t.(string); ok {
						if n := normPhrase(s); n != "" {
							to = append(to, n)
						}
					}
				}
			}
			if from != "" && len(to) > 0 {
				c.OneWay = append(c.OneWay, OneWayRule{From: from, To: to})
			}
		}
	}
	if s, ok := m["numberRoman"].(string); ok {
		c.NumberRoman = s
	}
	if s, ok := m["numberWords"].(string); ok {
		c.NumberWords = s
	}
	if c.empty() {
		return nil
	}
	return c
}

func normPhrase(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// buildSynDict compiles the rules into key -> alternatives (never itself).
func buildSynDict(c *SynonymConfig) map[string][]string {
	if c == nil {
		return nil
	}
	set := map[string]map[string]bool{}
	add := func(from, to string) {
		if from == to {
			return
		}
		if set[from] == nil {
			set[from] = map[string]bool{}
		}
		set[from][to] = true
	}
	for _, eq := range c.Equivalents {
		for _, a := range eq {
			for _, b := range eq {
				add(a, b)
			}
		}
	}
	for _, r := range c.OneWay {
		for _, t := range r.To {
			add(r.From, t)
		}
	}
	out := map[string][]string{}
	for k, v := range set {
		for t := range v {
			out[k] = append(out[k], t)
		}
	}
	return out
}

// synAlts returns the alternatives (dictionary + computed numerals) for a normalized
// single term, excluding the term itself. Directionality matches the hosted service: `oneway`
// expands only digit -> form; `twoway` also expands form -> digit. The two edges are
// independent and never chain (a generated form is not re-expanded).
func (ix *Index) synAlts(term string) []string {
	if ix.syn == nil {
		return nil
	}
	key := normPhrase(term)
	seen := map[string]bool{key: true}
	var out []string
	push := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, a := range ix.synDict[key] {
		push(a)
	}
	roman, words := ix.syn.NumberRoman, ix.syn.NumberWords
	if n, err := strconv.Atoi(key); err == nil && n >= 1 && n <= 3999 {
		// digit source: forward direction of each enabled edge
		if roman == "oneway" || roman == "twoway" {
			push(intToRoman(n))
		}
		if words == "oneway" || words == "twoway" {
			push(intToWords(n))
		}
	} else if n := romanToInt(key); n > 0 && roman == "twoway" {
		push(strconv.Itoa(n))
	} else if n := wordsToInt(strings.Fields(key)); n > 0 && words == "twoway" {
		push(strconv.Itoa(n))
	}
	return out
}

// ---- numeral codecs (digit <-> roman <-> words; range 1..3999) ----

var romanTable = []struct {
	v   int
	sym string
}{{1000, "m"}, {900, "cm"}, {500, "d"}, {400, "cd"}, {100, "c"}, {90, "xc"},
	{50, "l"}, {40, "xl"}, {10, "x"}, {9, "ix"}, {5, "v"}, {4, "iv"}, {1, "i"}}

func intToRoman(n int) string {
	if n < 1 || n > 3999 {
		return ""
	}
	var b strings.Builder
	for _, e := range romanTable {
		for n >= e.v {
			b.WriteString(e.sym)
			n -= e.v
		}
	}
	return b.String()
}

var romanVal = map[byte]int{'i': 1, 'v': 5, 'x': 10, 'l': 50, 'c': 100, 'd': 500, 'm': 1000}

// romanToInt parses a strictly CANONICAL roman numeral, or 0. Single letters are
// rejected (so "iPhone X" / "Vitamin C" never convert), and the value must round-trip.
func romanToInt(s string) int {
	t := strings.ToLower(s)
	if len(t) < 2 {
		return 0
	}
	val := 0
	for i := 0; i < len(t); i++ {
		cur, ok := romanVal[t[i]]
		if !ok {
			return 0
		}
		next := 0
		if i+1 < len(t) {
			next = romanVal[t[i+1]]
		}
		if cur < next {
			val -= cur
		} else {
			val += cur
		}
	}
	if val < 1 || val > 3999 || intToRoman(val) != t {
		return 0
	}
	return val
}

var onesWords = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
	"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen"}
var tensWords = []string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}

// intToWords renders the single canonical American spelling (no "and").
func intToWords(n int) string {
	if n < 1 || n > 3999 {
		return ""
	}
	var parts []string
	if n >= 1000 {
		parts = append(parts, onesWords[n/1000], "thousand")
		n %= 1000
	}
	if n >= 100 {
		parts = append(parts, onesWords[n/100], "hundred")
		n %= 100
	}
	if n >= 20 {
		parts = append(parts, tensWords[n/10])
		n %= 10
		if n > 0 {
			parts = append(parts, onesWords[n])
		}
	} else if n > 0 {
		parts = append(parts, onesWords[n])
	}
	return strings.Join(parts, " ")
}

var wordVal = func() map[string]int {
	m := map[string]int{}
	for i, w := range onesWords {
		m[w] = i
	}
	for i, w := range tensWords {
		if w != "" {
			m[w] = i * 10
		}
	}
	return m
}()

// wordsToInt parses a canonical spelled-out run to its int in 1..3999, or 0.
func wordsToInt(tokens []string) int {
	if len(tokens) == 0 {
		return 0
	}
	total, current := 0, 0
	for _, raw := range tokens {
		w := strings.ToLower(raw)
		switch w {
		case "hundred":
			if current == 0 || current > 9 {
				return 0
			}
			current *= 100
		case "thousand":
			if current == 0 || current > 999 {
				return 0
			}
			total += current * 1000
			current = 0
		default:
			v, ok := wordVal[w]
			if !ok {
				return 0
			}
			if current%100 != 0 && current != 0 {
				if !(current%10 == 0 && current >= 20 && v >= 1 && v <= 9) {
					return 0
				}
			}
			current += v
		}
	}
	n := total + current
	if n < 1 || n > 3999 {
		return 0
	}
	return n
}

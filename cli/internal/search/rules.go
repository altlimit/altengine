package search

import (
	"encoding/json"
	"fmt"
	"strings"
)

func docFromBody(body string) *Document {
	var d Document
	if json.Unmarshal([]byte(body), &d) != nil {
		return nil
	}
	return &d
}

// Query rules (merchandising), matching the hosted service: when a rule's
// pattern matches the RAW query (before synonym expansion), it can PIN documents to
// absolute positions, HIDE documents, and attach free-form DATA echoed to the response.
// Hide beats pin; a pin naming a nonexistent document is skipped; the first rule to pin
// a document wins its position.

const (
	maxRulePins  = 20
	maxRuleHides = 20
)

type RulePin struct {
	ID  string
	Pos int
}

type SearchRule struct {
	WhenQuery string
	WhenMatch string // is | contains
	Pin       []RulePin
	Hide      []string
	Data      any
}

// parseRules tolerantly reads the stored config value (decoded JSON array).
func parseRules(v any) []SearchRule {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []SearchRule
	for _, rv := range arr {
		rm, ok := rv.(map[string]any)
		if !ok {
			continue
		}
		when, _ := rm["when"].(map[string]any)
		then, _ := rm["then"].(map[string]any)
		if when == nil || then == nil {
			continue
		}
		q, _ := when["query"].(string)
		q = normPhrase(q)
		if q == "" {
			continue
		}
		match, _ := when["match"].(string)
		if match != "contains" {
			match = "is"
		}
		r := SearchRule{WhenQuery: q, WhenMatch: match}
		if pins, ok := then["pin"].([]any); ok {
			for _, pv := range pins {
				pm, ok := pv.(map[string]any)
				if !ok {
					continue
				}
				id, _ := pm["id"].(string)
				pos, _ := pm["pos"].(float64)
				if id != "" && pos >= 0 {
					r.Pin = append(r.Pin, RulePin{ID: id, Pos: int(pos)})
				}
			}
		}
		if hides, ok := then["hide"].([]any); ok {
			for _, hv := range hides {
				if id, ok := hv.(string); ok && id != "" {
					r.Hide = append(r.Hide, id)
				}
			}
		}
		r.Data = then["data"]
		if len(r.Pin) == 0 && len(r.Hide) == 0 && r.Data == nil {
			continue
		}
		out = append(out, r)
	}
	return out
}

type appliedRules struct {
	Pins []RulePin
	Hide []string
	Data []any
}

// matchRules evaluates the rules against the raw query text. `is` = whole-query match,
// `contains` = word-boundary phrase containment.
func matchRules(rules []SearchRule, query string) appliedRules {
	var out appliedRules
	if len(rules) == 0 {
		return out
	}
	q := normPhrase(query)
	hidden := map[string]bool{}
	pinned := map[string]bool{}
	for _, r := range rules {
		hit := false
		if r.WhenMatch == "is" {
			hit = q == r.WhenQuery
		} else {
			hit = containsWords(q, r.WhenQuery)
		}
		if !hit {
			continue
		}
		for _, h := range r.Hide {
			if !hidden[h] && len(out.Hide) < maxRuleHides {
				hidden[h] = true
				out.Hide = append(out.Hide, h)
			}
		}
		for _, p := range r.Pin {
			// first rule to pin a document wins its position
			if !pinned[p.ID] && len(out.Pins) < maxRulePins {
				pinned[p.ID] = true
				out.Pins = append(out.Pins, p)
			}
		}
		if r.Data != nil {
			out.Data = append(out.Data, r.Data)
		}
	}
	// hide beats pin
	var pins []RulePin
	for _, p := range out.Pins {
		if !hidden[p.ID] {
			pins = append(pins, p)
		}
	}
	// sort by requested position (stable insertion order for ties)
	for i := 1; i < len(pins); i++ {
		for j := i; j > 0 && pins[j].Pos < pins[j-1].Pos; j-- {
			pins[j], pins[j-1] = pins[j-1], pins[j]
		}
	}
	out.Pins = pins
	return out
}

// containsWords reports whether `phrase` occurs in `q` on word boundaries
// ("cat" matches "cat food" but not "concatenate").
func containsWords(q, phrase string) bool {
	qw := strings.Fields(q)
	pw := strings.Fields(phrase)
	if len(pw) == 0 || len(pw) > len(qw) {
		return false
	}
	for i := 0; i+len(pw) <= len(qw); i++ {
		ok := true
		for j := range pw {
			if qw[i+j] != pw[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// excludeIDs appends an EXCEPT of literal document ids to a doc_id set expression.
func excludeIDs(setSQLText string, args []any, ids []string) (string, []any) {
	if len(ids) == 0 {
		return setSQLText, args
	}
	vals := make([]string, len(ids))
	for i, id := range ids {
		vals[i] = "(?)"
		args = append(args, id)
	}
	return "SELECT doc_id FROM (" + setSQLText + ") EXCEPT VALUES " + strings.Join(vals, ","), args
}

// fetchPinned loads the pinned documents that actually exist, in pin order. A pin
// naming a missing document is skipped (a "dead pin" never blanks a slot).
func (ix *Index) fetchPinned(s *Store, pins []RulePin) ([]SearchResult, []RulePin, error) {
	if len(pins) == 0 {
		return nil, nil, nil
	}
	ph := make([]string, len(pins))
	args := make([]any, len(pins))
	for i, p := range pins {
		ph[i] = "?"
		args[i] = p.ID
	}
	rows, err := s.db.Query(fmt.Sprintf(`SELECT doc_id, rank, body FROM %s_docs WHERE doc_id IN (%s)`,
		ix.prefix, strings.Join(ph, ",")), args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byID := map[string]SearchResult{}
	for rows.Next() {
		var id, body string
		var rank int64
		if err := rows.Scan(&id, &rank, &body); err != nil {
			return nil, nil, err
		}
		byID[id] = SearchResult{ID: id, Rank: rank, Document: docFromBody(body)}
	}
	var results []SearchResult
	var live []RulePin
	for _, p := range pins {
		if r, ok := byID[p.ID]; ok {
			results = append(results, r)
			live = append(live, p)
		}
	}
	return results, live, rows.Err()
}

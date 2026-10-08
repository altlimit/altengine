package search

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
)

// Write-path limits, as hosted.
const (
	maxSynonymRules   = 200
	maxSynonymMembers = 20
	maxSynonymWords   = 5
	maxSynonymBytes   = 16 * 1024
	maxQueryRules     = 200
	maxRulePos        = 1000
	maxRulesBytes     = 16 * 1024
)

// ValidateConfig is the search instance's config check on a write, with the hosted rules.
func ValidateConfig(cfg map[string]any) error {
	if v, present := cfg["stemming"]; present {
		if _, ok := v.(bool); !ok {
			return common.BadRequest("config.stemming must be a boolean")
		}
	}
	return common.FirstErr(
		common.CheckRateLimit(cfg, "rateLimit"),
		common.CheckRegion(cfg),
		validateSynonyms(cfg["synonyms"]),
		validateRules(cfg["rules"]),
	)
}

func validateSynonyms(v any) error {
	if v == nil {
		return nil
	}
	o, ok := v.(map[string]any)
	if !ok {
		return common.BadRequest("config.synonyms must be an object")
	}
	for _, k := range []string{"numberRoman", "numberWords"} {
		if d, present := o[k]; present && d != nil {
			if s, _ := d.(string); s != "off" && s != "oneway" && s != "twoway" {
				return common.BadRequest(fmt.Sprintf("config.synonyms.%s must be one of off, oneway, twoway", k))
			}
		}
	}
	phrase := func(m any, what string) error {
		s, ok := m.(string)
		if !ok {
			return common.BadRequest(what + " must be a string")
		}
		words := strings.Fields(normPhrase(s))
		if len(words) == 0 {
			return common.BadRequest(what + " must not be empty")
		}
		if len(words) > maxSynonymWords {
			return common.BadRequest(fmt.Sprintf("%s exceeds %d words", what, maxSynonymWords))
		}
		return nil
	}
	rules := 0
	if eq, present := o["equivalents"]; present && eq != nil {
		sets, ok := eq.([]any)
		if !ok {
			return common.BadRequest("config.synonyms.equivalents must be an array")
		}
		for _, s := range sets {
			set, ok := s.([]any)
			if !ok {
				return common.BadRequest("each synonyms.equivalents entry must be an array")
			}
			if len(set) < 2 {
				return common.BadRequest("an equivalents set needs at least 2 members")
			}
			if len(set) > maxSynonymMembers {
				return common.BadRequest(fmt.Sprintf("an equivalents set exceeds %d members", maxSynonymMembers))
			}
			for _, m := range set {
				if err := phrase(m, "synonyms.equivalents member"); err != nil {
					return err
				}
			}
			rules++
		}
	}
	if ow, present := o["oneWay"]; present && ow != nil {
		list, ok := ow.([]any)
		if !ok {
			return common.BadRequest("config.synonyms.oneWay must be an array")
		}
		for _, r := range list {
			rr, ok := r.(map[string]any)
			if !ok {
				return common.BadRequest("each synonyms.oneWay entry must be an object")
			}
			to, ok := rr["to"].([]any)
			if !ok {
				return common.BadRequest("synonyms.oneWay.to must be an array")
			}
			if len(to) < 1 {
				return common.BadRequest("synonyms.oneWay.to needs at least 1 member")
			}
			if len(to) > maxSynonymMembers {
				return common.BadRequest(fmt.Sprintf("synonyms.oneWay.to exceeds %d members", maxSynonymMembers))
			}
			if err := phrase(rr["from"], "synonyms.oneWay.from"); err != nil {
				return err
			}
			for _, m := range to {
				if err := phrase(m, "synonyms.oneWay.to member"); err != nil {
					return err
				}
			}
			rules++
		}
	}
	if rules > maxSynonymRules {
		return common.BadRequest(fmt.Sprintf("config.synonyms exceeds %d rules", maxSynonymRules))
	}
	if b, _ := json.Marshal(o); len(b) > maxSynonymBytes {
		return common.BadRequest(fmt.Sprintf("config.synonyms is %d bytes, over the %d limit", len(b), maxSynonymBytes))
	}
	return nil
}

func validateRules(v any) error {
	if v == nil {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return common.BadRequest("config.rules must be an array")
	}
	if len(list) > maxQueryRules {
		return common.BadRequest(fmt.Sprintf("config.rules exceeds %d rules", maxQueryRules))
	}
	for _, r := range list {
		rr, ok := r.(map[string]any)
		if !ok {
			return common.BadRequest("each rule must be an object")
		}
		when, ok := rr["when"].(map[string]any)
		if !ok {
			return common.BadRequest("rule.when must be an object")
		}
		q, ok := when["query"].(string)
		if !ok {
			return common.BadRequest("rule.when.query must be a string")
		}
		if normPhrase(q) == "" {
			return common.BadRequest("rule.when.query must not be empty")
		}
		if m, present := when["match"]; present && m != nil && m != "is" && m != "contains" {
			return common.BadRequest(`rule.when.match must be "is" or "contains"`)
		}
		then, ok := rr["then"].(map[string]any)
		if !ok {
			return common.BadRequest("rule.then must be an object")
		}
		pins, hides := 0, 0
		if p, present := then["pin"]; present && p != nil {
			arr, ok := p.([]any)
			if !ok {
				return common.BadRequest("rule.then.pin must be an array")
			}
			if len(arr) > maxRulePins {
				return common.BadRequest(fmt.Sprintf("rule.then.pin exceeds %d documents", maxRulePins))
			}
			for _, e := range arr {
				pm, ok := e.(map[string]any)
				if !ok {
					return common.BadRequest("each rule.then.pin entry must be an object")
				}
				if id, ok := pm["id"].(string); !ok {
					return common.BadRequest("rule.then.pin.id must be a string")
				} else if id == "" {
					return common.BadRequest("rule.then.pin.id must not be empty")
				}
				if pos, present := pm["pos"]; present && pos != nil {
					if n, ok := common.Int(pos); !ok || n < 0 || n > maxRulePos {
						return common.BadRequest(fmt.Sprintf("rule.then.pin.pos must be an integer in 0..%d", maxRulePos))
					}
				}
			}
			pins = len(arr)
		}
		if h, present := then["hide"]; present && h != nil {
			arr, ok := h.([]any)
			if !ok {
				return common.BadRequest("rule.then.hide must be an array")
			}
			if len(arr) > maxRuleHides {
				return common.BadRequest(fmt.Sprintf("rule.then.hide exceeds %d documents", maxRuleHides))
			}
			for _, e := range arr {
				if _, ok := e.(string); !ok {
					return common.BadRequest("rule.then.hide entry must be a string")
				}
			}
			hides = len(arr)
		}
		if _, hasData := then["data"]; pins == 0 && hides == 0 && !hasData {
			return common.BadRequest("rule.then must do something: pin, hide, or data")
		}
	}
	if b, _ := json.Marshal(list); len(b) > maxRulesBytes {
		return common.BadRequest(fmt.Sprintf("config.rules is %d bytes, over the %d limit — rules are read on every search", len(b), maxRulesBytes))
	}
	return nil
}

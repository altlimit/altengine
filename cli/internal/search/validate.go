package search

import "github.com/altlimit/altengine/cli/internal/common"

// ValidateSearchRequest rejects a search body whose shape the engine cannot walk — `query: 5`,
// `sort: [null]`, `facets: "x"`, `facet_refinements: [null]` — as a 400 INVALID_ARGUMENT with
// the hosted service's messages. Scalar knobs (limit, depths, …) are clamped where read.
func ValidateSearchRequest(v any) error {
	r, ok := v.(map[string]any)
	if !ok {
		return common.BadRequest("search body must be a JSON object")
	}
	if q := r["query"]; q != nil {
		if _, ok := q.(string); !ok {
			return common.BadRequest("query must be a string")
		}
	}
	if c := r["cursor"]; c != nil {
		if _, ok := c.(string); !ok {
			return common.BadRequest("invalid cursor")
		}
	}
	for _, name := range []string{"facets", "returned_fields"} {
		if r[name] == nil {
			continue
		}
		arr, ok := r[name].([]any)
		if ok {
			for _, s := range arr {
				if _, isStr := s.(string); !isStr {
					ok = false
				}
			}
		}
		if !ok {
			return common.BadRequest(name + " must be an array of strings")
		}
	}
	if s := r["sort"]; s != nil {
		arr, ok := s.([]any)
		if !ok {
			return common.BadRequest("sort must be an array")
		}
		for _, e := range arr {
			m, ok := e.(map[string]any)
			if _, isStr := m["expr"].(string); !ok || !isStr {
				return common.BadRequest("each sort entry must be an object with a string 'expr'")
			}
		}
	}
	if f := r["facet_refinements"]; f != nil {
		arr, ok := f.([]any)
		if !ok {
			return common.BadRequest("facet_refinements must be an array")
		}
		for _, e := range arr {
			m, ok := e.(map[string]any)
			if _, isStr := m["name"].(string); !ok || !isStr {
				return common.BadRequest("each facet refinement must be an object with a string 'name'")
			}
			if v := m["value"]; v != nil {
				if _, ok := v.(string); !ok {
					return common.BadRequest("facet refinement 'value' must be a string")
				}
			}
			for _, k := range []string{"min", "max"} {
				if v := m[k]; v != nil {
					if _, ok := v.(float64); !ok {
						return common.BadRequest("facet refinement '" + k + "' must be a finite number")
					}
				}
			}
		}
	}
	return nil
}

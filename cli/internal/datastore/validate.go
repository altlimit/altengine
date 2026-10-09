package datastore

import (
	"fmt"

	"github.com/altlimit/altengine/cli/internal/common"
)

// Request shape, checked before a body is decoded. A query or aggregate body is client JSON;
// a shape the engine cannot walk (`where: "x"`, `order: {}`, `metrics: [null]`, a numeric
// field path, a null body) is a 400 INVALID_ARGUMENT naming the field, with the same
// messages as the hosted service.

func isObj(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func optArray(v any, name string) ([]any, error) {
	if v == nil {
		return nil, nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil, common.BadRequest(fmt.Sprintf("'%s' must be an array", name))
	}
	return a, nil
}

func eachObj(v any, name string) ([]map[string]any, error) {
	a, err := optArray(v, name)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(a))
	for i, e := range a {
		m, ok := isObj(e)
		if !ok {
			return nil, common.BadRequest(fmt.Sprintf("'%s[%d]' must be an object", name, i))
		}
		out = append(out, m)
	}
	return out, nil
}

func strField(o map[string]any, key, where string) error {
	if _, ok := o[key].(string); !ok {
		return common.BadRequest(fmt.Sprintf("'%s.%s' must be a string", where, key))
	}
	return nil
}

func validateWhereOrder(req map[string]any) error {
	where, err := eachObj(req["where"], "where")
	if err != nil {
		return err
	}
	for i, f := range where {
		for _, k := range []string{"field", "op"} {
			if err := strField(f, k, fmt.Sprintf("where[%d]", i)); err != nil {
				return err
			}
		}
	}
	order, err := eachObj(req["order"], "order")
	if err != nil {
		return err
	}
	for i, o := range order {
		if err := strField(o, "field", fmt.Sprintf("order[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

// ValidateQueryRequest rejects a query body whose shape the engine cannot walk.
func ValidateQueryRequest(v any) error {
	req, ok := isObj(v)
	if !ok {
		return common.BadRequest("query body must be a JSON object")
	}
	if err := validateWhereOrder(req); err != nil {
		return err
	}
	if c := req["cursor"]; c != nil {
		if _, ok := c.(string); !ok {
			return common.BadRequest("'cursor' must be a string")
		}
	}
	joins, err := eachObj(req["join"], "join")
	if err != nil {
		return err
	}
	for i, j := range joins {
		for _, k := range []string{"as", "collection", "local_field"} {
			if err := strField(j, k, fmt.Sprintf("join[%d]", i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateAggregateRequest rejects an aggregate body whose shape the engine cannot walk.
func ValidateAggregateRequest(v any) error {
	req, ok := isObj(v)
	if !ok {
		return common.BadRequest("aggregate body must be a JSON object")
	}
	if err := validateWhereOrder(req); err != nil {
		return err
	}
	group, err := optArray(req["group"], "group")
	if err != nil {
		return err
	}
	for i, g := range group {
		if _, ok := g.(string); !ok {
			return common.BadRequest(fmt.Sprintf("'group[%d]' must be a string", i))
		}
	}
	metrics, err := eachObj(req["metrics"], "metrics")
	if err != nil {
		return err
	}
	for i, m := range metrics {
		where := fmt.Sprintf("metrics[%d]", i)
		if err := strField(m, "fn", where); err != nil {
			return err
		}
		if m["field"] != nil {
			if err := strField(m, "field", where); err != nil {
				return err
			}
		}
	}
	return nil
}

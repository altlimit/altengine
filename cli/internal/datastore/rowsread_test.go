package datastore

import "testing"

// A query's response carries no rowsRead: it is metering plumbing, not part of the result.
func TestQueryResponseCarriesNoRowsRead(t *testing.T) {
	e := newRuleEnv(t)
	if status, out := e.call(t, "/secrets/documents", `{"documents":[{"key":"s1","data":{"x":1}}]}`, "devkey"); status != 200 {
		t.Fatalf("put -> %d %v", status, out)
	}
	status, out := e.call(t, "/secrets/query", `{}`, "devkey")
	if status != 200 {
		t.Fatalf("query -> %d %v", status, out)
	}
	if _, ok := out["rowsRead"]; ok {
		t.Fatalf("query response carries rowsRead: %v", out)
	}
	if docs, _ := out["documents"].([]any); len(docs) != 1 {
		t.Fatalf("documents = %v", out["documents"])
	}
}

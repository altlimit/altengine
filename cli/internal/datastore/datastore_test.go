package datastore

import (
	"encoding/json"
	"testing"
)

func mustPut(t *testing.T, s *Store, coll string, docs ...string) {
	t.Helper()
	var pd []PutDoc
	for i := 0; i < len(docs); i += 2 {
		pd = append(pd, PutDoc{Key: docs[i], Data: json.RawMessage(docs[i+1])})
	}
	if _, _, err := s.Put(coll, pd); err != nil {
		t.Fatalf("put: %v", err)
	}
}

func openTest(t *testing.T) *Store {
	t.Helper()
	m := NewManager("")
	s, err := m.Open("inst-"+t.Name(), "ns", "uuid")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestQueryAutoIndexAndFilter(t *testing.T) {
	s := openTest(t)
	mustPut(t, s, "users",
		"u1", `{"role":"admin","age":40}`,
		"u2", `{"role":"user","age":20}`,
		"u3", `{"role":"user","age":33}`)

	res, err := s.Query("users", QueryRequest{Where: []Filter{{Field: "role", Op: "=", Value: "user"}}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Documents) != 2 {
		t.Fatalf("expected 2 users, got %d", len(res.Documents))
	}
	if res.AutoIndexed == nil {
		t.Fatalf("expected auto_indexed to be set")
	}
}

func TestIndexRequiredWhenAutoIndexOff(t *testing.T) {
	s := openTest(t)
	mustPut(t, s, "p", "a", `{"x":1}`)
	_, err := s.Query("p", QueryRequest{Where: []Filter{{Field: "x", Op: "=", Value: float64(1)}}}, false)
	if err == nil {
		t.Fatalf("expected INDEX_REQUIRED error")
	}
}

func TestTransactionMutateIncrement(t *testing.T) {
	s := openTest(t)
	mustPut(t, s, "c", "k", `{"n":1}`)
	_, _, err := s.RunTransaction([]TxnOp{
		{Op: "mutate", Collection: "c", Key: "k", Increment: map[string]float64{"n": 5}},
		{Op: "check", Collection: "c", Key: "k", Exists: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := s.Get("c", "k")
	var m map[string]any
	json.Unmarshal(doc.Data, &m)
	if m["n"].(float64) != 6 {
		t.Fatalf("expected n=6, got %v", m["n"])
	}
}

func TestUniqueIndexViolation(t *testing.T) {
	s := openTest(t)
	if _, err := s.CreateIndex("e", []string{"email"}, true); err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, "e", "a", `{"email":"x@y.com"}`)
	_, _, err := s.Put("e", []PutDoc{{Key: "b", Data: json.RawMessage(`{"email":"x@y.com"}`)}})
	if err == nil {
		t.Fatalf("expected unique violation")
	}
}

//go:build conformance

package altengine_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"

	altengine "github.com/altlimit/altengine/go"
)

type dsSeed struct {
	Collection string `json:"collection"`
	Documents  []struct {
		Key  string         `json:"key"`
		Data map[string]any `json:"data"`
	} `json:"documents"`
	Owners []struct {
		Key  string         `json:"key"`
		Data map[string]any `json:"data"`
	} `json:"owners"`
}

func seedDatastore(t *testing.T) (*altengine.Datastore, dsSeed) {
	t.Helper()
	var seed dsSeed
	loadFixture(t, "datastore-seed.json", &seed)
	db := client().Datastore(uniq("sdk-conf")).WithNamespace(uniq("ns"))
	var docs, owners []altengine.PutDocument
	for _, d := range seed.Documents {
		docs = append(docs, altengine.PutDocument{Key: d.Key, Data: d.Data})
	}
	for _, o := range seed.Owners {
		owners = append(owners, altengine.PutDocument{Key: o.Key, Data: o.Data})
	}
	if _, err := db.Put(ctx(t), seed.Collection, docs); err != nil {
		t.Fatalf("seeding todos: %v", err)
	}
	if _, err := db.Put(ctx(t), "owners", owners); err != nil {
		t.Fatalf("seeding owners: %v", err)
	}
	return db, seed
}

func TestDatastoreCRUD(t *testing.T) {
	db, seed := seedDatastore(t)

	t.Run("put get roundtrip preserves data and reports timestamps", func(t *testing.T) {
		doc, err := db.Get(ctx(t), "todos", "t1")
		if err != nil || doc == nil {
			t.Fatalf("get t1: doc=%v err=%v", doc, err)
		}
		var data struct {
			Title string `json:"title"`
		}
		if err := doc.DataAs(&data); err != nil || data.Title != "ship SDK" {
			t.Fatalf("data roundtrip: %+v err=%v", data, err)
		}
		if doc.Created <= 0 || doc.Updated < doc.Created {
			t.Fatalf("timestamps: created=%d updated=%d", doc.Created, doc.Updated)
		}
	})

	t.Run("puts are upserts and preserve created", func(t *testing.T) {
		before, _ := db.Get(ctx(t), "todos", "t1")
		if _, err := db.Put(ctx(t), "todos", []altengine.PutDocument{
			{Key: "t1", Data: map[string]any{"title": "ship SDK v2", "owner": "ana", "done": false, "priority": 1}},
		}); err != nil {
			t.Fatal(err)
		}
		after, _ := db.Get(ctx(t), "todos", "t1")
		var data struct {
			Title string `json:"title"`
		}
		after.DataAs(&data)
		if data.Title != "ship SDK v2" {
			t.Fatalf("title = %q", data.Title)
		}
		if after.Created != before.Created {
			t.Fatalf("created changed: %d != %d", after.Created, before.Created)
		}
		// restore
		for _, d := range seed.Documents {
			if d.Key == "t1" {
				db.Put(ctx(t), "todos", []altengine.PutDocument{{Key: d.Key, Data: d.Data}})
			}
		}
	})

	t.Run("auto-id put returns a generated key; numeric keys coerce to strings", func(t *testing.T) {
		keys, err := db.Put(ctx(t), "todos", []altengine.PutDocument{
			{Data: map[string]any{"title": "auto"}},
			{Key: 42, Data: map[string]any{"title": "num"}},
		})
		if err != nil || len(keys) != 2 {
			t.Fatalf("keys=%v err=%v", keys, err)
		}
		if keys[0] == "" || keys[1] != "42" {
			t.Fatalf("keys=%v", keys)
		}
		doc, _ := db.Get(ctx(t), "todos", "42")
		if doc == nil {
			t.Fatal("numeric-keyed doc missing")
		}
		anyKeys := make([]any, len(keys))
		for i, k := range keys {
			anyKeys[i] = k
		}
		db.Delete(ctx(t), "todos", anyKeys)
	})

	t.Run("GetMulti is order-preserving with nils for missing keys", func(t *testing.T) {
		docs, err := db.GetMulti(ctx(t), "todos", []any{"t1", "does-not-exist", "t3"})
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(docs))
		for i, d := range docs {
			if d != nil {
				got[i] = d.Key
			}
		}
		if !reflect.DeepEqual(got, []string{"t1", "", "t3"}) {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("get of missing key returns nil; delete of missing is a no-op", func(t *testing.T) {
		doc, err := db.Get(ctx(t), "todos", "ghost")
		if err != nil || doc != nil {
			t.Fatalf("doc=%v err=%v", doc, err)
		}
		if _, err := db.Delete(ctx(t), "todos", []any{"ghost"}); err != nil {
			t.Fatalf("delete missing: %v", err)
		}
	})
}

func TestDatastoreQuery(t *testing.T) {
	db, _ := seedDatastore(t)

	t.Run("filters with = and orders desc with cursor pagination", func(t *testing.T) {
		var all []string
		req := altengine.QueryRequest{
			Where: []altengine.Filter{{Field: "done", Op: "=", Value: false}},
			Order: []altengine.Order{{Field: "priority", Dir: "desc"}},
			Limit: 1,
		}
		for {
			page, err := db.Query(ctx(t), "todos", req)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range page.Documents {
				all = append(all, d.Key)
			}
			if page.Cursor == nil {
				break
			}
			req.Cursor = *page.Cursor
		}
		if !reflect.DeepEqual(all, []string{"t4", "t3", "t1"}) {
			t.Fatalf("got %v", all)
		}
	})

	t.Run("supports in, dot-paths, and keys_only", func(t *testing.T) {
		res, err := db.Query(ctx(t), "todos", altengine.QueryRequest{
			Where:    []altengine.Filter{{Field: "meta.tag", Op: "in", Value: []string{"home"}}},
			KeysOnly: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		keys := append([]string(nil), res.Keys...)
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, []string{"t3", "t5"}) {
			t.Fatalf("got %v", keys)
		}
	})

	t.Run("QueryAll iterates to exhaustion", func(t *testing.T) {
		var keys []string
		for doc, err := range db.QueryAll(ctx(t), "todos", altengine.QueryRequest{
			Where: []altengine.Filter{{Field: "owner", Op: "=", Value: "ana"}},
			Limit: 1,
		}) {
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, doc.Key)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, []string{"t1", "t2"}) {
			t.Fatalf("got %v", keys)
		}
	})

	t.Run("join attaches the referenced document", func(t *testing.T) {
		res, err := db.Query(ctx(t), "todos", altengine.QueryRequest{
			Where: []altengine.Filter{{Field: "__key__", Op: "=", Value: "t1"}},
			Join:  []altengine.Join{{As: "owner_doc", Collection: "owners", LocalField: "owner"}},
		})
		if err != nil || len(res.Documents) != 1 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		var joined struct {
			Data struct {
				Name string `json:"name"`
			} `json:"data"`
		}
		if err := json.Unmarshal(res.Documents[0].Joins["owner_doc"], &joined); err != nil || joined.Data.Name != "Ana" {
			t.Fatalf("join: %+v err=%v", joined, err)
		}
	})

	t.Run("aggregates count and sum with grouping", func(t *testing.T) {
		res, err := db.Aggregate(ctx(t), "todos", altengine.AggregateRequest{
			Group: []string{"owner"},
			Metrics: []altengine.Metric{
				{Fn: "count", As: "n"},
				{Fn: "sum", Field: "priority", As: "total"},
			},
			Order: []altengine.Order{{Field: "owner", Dir: "asc"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		var ana *altengine.AggregateGroup
		for n := range res.Groups {
			if res.Groups[n].Group["owner"] == "ana" {
				ana = &res.Groups[n]
			}
		}
		if ana == nil || *ana.Metrics["n"] != 2 || *ana.Metrics["total"] != 3 {
			t.Fatalf("ana group: %+v", ana)
		}
	})
}

func TestDatastoreTransactionsAndIndexes(t *testing.T) {
	db, _ := seedDatastore(t)

	t.Run("applies put mutate check atomically", func(t *testing.T) {
		db.Put(ctx(t), "counters", []altengine.PutDocument{{Key: "c1", Data: map[string]any{"total": 0}}})
		keys, err := db.Transaction(ctx(t), []altengine.TxnOp{
			altengine.TxnCheck("counters", "c1", true),
			altengine.TxnMutate("counters", "c1", altengine.TxnOp{Increment: map[string]float64{"total": 5}}),
			altengine.TxnPut("counters", "c2", map[string]any{"total": 1}),
		})
		if err != nil || len(keys) != 3 {
			t.Fatalf("keys=%v err=%v", keys, err)
		}
		doc, _ := db.Get(ctx(t), "counters", "c1")
		var data struct {
			Total float64 `json:"total"`
		}
		doc.DataAs(&data)
		if data.Total != 5 {
			t.Fatalf("total=%v", data.Total)
		}
	})

	t.Run("failed check aborts with 409 and applies nothing", func(t *testing.T) {
		_, err := db.Transaction(ctx(t), []altengine.TxnOp{
			altengine.TxnCheck("counters", "ghost", true),
			altengine.TxnMutate("counters", "c1", altengine.TxnOp{Increment: map[string]float64{"total": 100}}),
		})
		var ae *altengine.APIError
		if !errors.As(err, &ae) || ae.Status != 409 {
			t.Fatalf("err=%v", err)
		}
		doc, _ := db.Get(ctx(t), "counters", "c1")
		var data struct {
			Total float64 `json:"total"`
		}
		doc.DataAs(&data)
		if data.Total != 5 {
			t.Fatalf("total=%v after aborted txn", data.Total)
		}
	})

	t.Run("index create is idempotent; unique index rejects duplicates", func(t *testing.T) {
		a, err := db.CreateIndex(ctx(t), "users", []string{"email"}, true)
		if err != nil || a == nil {
			t.Fatalf("create: %v", err)
		}
		b, err := db.CreateIndex(ctx(t), "users", []string{"email"}, true)
		if err != nil || b.ID != a.ID {
			t.Fatalf("idempotency: a=%v b=%v err=%v", a, b, err)
		}
		if _, err := db.Put(ctx(t), "users", []altengine.PutDocument{{Key: "u1", Data: map[string]any{"email": "x@y.z"}}}); err != nil {
			t.Fatal(err)
		}
		_, err = db.Put(ctx(t), "users", []altengine.PutDocument{{Key: "u2", Data: map[string]any{"email": "x@y.z"}}})
		var ae *altengine.APIError
		if !errors.As(err, &ae) {
			t.Fatalf("unique violation not an APIError: %v", err)
		}
		ix, err := db.ListIndexes(ctx(t), "users")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, spec := range ix {
			if spec.ID == a.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("index %d missing from list", a.ID)
		}
	})
}

func TestDatastoreNamespaces(t *testing.T) {
	db, _ := seedDatastore(t)

	other := db.WithNamespace(uniq("other"))
	if _, err := other.Put(ctx(t), "todos", []altengine.PutDocument{{Key: "only-here", Data: map[string]any{"a": 1}}}); err != nil {
		t.Fatal(err)
	}
	if doc, _ := db.Get(ctx(t), "todos", "only-here"); doc != nil {
		t.Fatal("namespace leak: doc visible in default namespace")
	}
	if doc, _ := other.Get(ctx(t), "todos", "only-here"); doc == nil {
		t.Fatal("doc missing in its own namespace")
	}

	page, err := db.ListNamespaces(ctx(t), altengine.ListOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ns := range page.Namespaces {
		if ns == other.Namespace {
			found = true
		}
	}
	if !found {
		t.Fatalf("namespace %q not listed in %v", other.Namespace, page.Namespaces)
	}

	if destructiveOk() {
		if _, err := db.DeleteNamespace(ctx(t), other.Namespace); err != nil {
			t.Fatal(err)
		}
		if doc, _ := other.Get(ctx(t), "todos", "only-here"); doc != nil {
			t.Fatal("doc survived namespace delete")
		}
	}
}

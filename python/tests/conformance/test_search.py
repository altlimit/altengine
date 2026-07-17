from __future__ import annotations

import pytest

from altengine import AltEngineError

from .conftest import load_fixture, uniq


@pytest.fixture(scope="module")
def s(client):
    return client.search(uniq("sdk-conf"))


@pytest.fixture(scope="module")
def idx(s):
    corpus = load_fixture("search-corpus.json")
    idx = s.index("products")
    ids = idx.put(corpus["documents"])
    assert ids == [d["id"] for d in corpus["documents"]]
    return idx


class TestDocuments:
    def test_get_roundtrips_all_field_types(self, idx):
        doc = idx.get("p1")
        assert doc is not None
        by_name = {f["name"]: f for f in doc["fields"]}
        assert by_name["title"]["value"] == "Blue Suede Shoes"
        assert by_name["price"]["value"] == 59
        assert by_name["store"]["value"]["lat"] == 37.77
        assert len(doc["facets"]) == 2

    def test_get_missing_returns_none(self, idx):
        assert idx.get("nope") is None

    def test_get_list_is_order_preserving_with_nones(self, idx):
        docs = idx.get(["p1", "does-not-exist", "p3"])
        assert [d["id"] if d else None for d in docs] == ["p1", None, "p3"]

    def test_server_assigns_ids(self, idx):
        ids = idx.put([{"fields": [{"name": "title", "type": "text", "value": "temp"}]}])
        assert ids[0]
        idx.delete(ids)

    def test_list_all_documents_keyset(self, idx):
        seen = [d["id"] for d in idx.list_all_documents(limit=2)]
        assert sorted(seen) == ["p1", "p2", "p3", "p4"]


class TestQueries:
    def test_bare_and_field_terms(self, idx):
        res = idx.search("shoes")
        assert sorted(h["id"] for h in res["results"]) == ["p1", "p2", "p4"]
        atom = idx.search('sku:"BS-001"')
        assert [h["id"] for h in atom["results"]] == ["p1"]

    def test_boolean_operators_and_comparisons(self, idx):
        res = idx.search("shoes AND price<100")
        assert sorted(h["id"] for h in res["results"]) == ["p1", "p4"]
        res = idx.search("shoes NOT blue")
        assert [h["id"] for h in res["results"]] == ["p2"]

    def test_sort_and_cursor_pagination(self, idx):
        page1 = idx.search("shoes", sort=[{"expr": "price"}], limit=2)
        assert [h["id"] for h in page1["results"]] == ["p4", "p1"]
        assert page1["cursor"]
        page2 = idx.search("shoes", sort=[{"expr": "price"}], limit=2, cursor=page1["cursor"])
        assert [h["id"] for h in page2["results"]] == ["p2"]

    def test_facets_and_refinements(self, idx):
        res = idx.search("", facets=["category"])
        cat = next(f for f in res["facets"] if f["name"] == "category")
        assert next(v for v in cat["values"] if v["value"] == "shoes")["count"] == 3
        refined = idx.search("", facet_refinements=[{"name": "category", "value": "accessories"}])
        assert [h["id"] for h in refined["results"]] == ["p3"]

    def test_snippets(self, idx):
        res = idx.search("suede", snippet={"fields": ["title"], "pre_tag": "<em>", "post_tag": "</em>"})
        assert "<em>" in res["results"][0]["snippet"]["title"]

    def test_collapse(self, idx):
        res = idx.search("shoes", collapse={"field": "category", "limit": 1})
        assert len(res["results"]) == 1

    def test_ids_only(self, idx):
        res = idx.search("shoes", ids_only=True)
        assert all("document" not in h for h in res["results"])

    def test_search_all_pages_cursors(self, idx):
        hits = list(idx.search_all("shoes", limit=1))
        assert len(hits) == 3


class TestIndexManagement:
    def test_schema_union(self, idx):
        schema = idx.schema()
        assert schema["name"] == "products"
        assert "number" in schema["fields"]["price"]

    def test_namespace_isolation_and_listing(self, s, idx, destructive_ok):
        other = s.with_namespace(uniq("ns"))
        other.index("products").put([{"id": "only", "fields": [{"name": "title", "type": "text", "value": "hidden"}]}])
        assert idx.get("only") is None
        assert any(ix["name"] == "products" for ix in other.list_indexes()["indexes"])

        page = s.list_namespaces(limit=100)
        assert other.namespace in page["namespaces"]
        assert "" in page["namespaces"]

        if destructive_ok:
            assert other.delete_index("products") is True

    def test_invalid_namespaces_rejected(self, s):
        for bad in ["a\x00b", "x" * 101, "café"]:
            with pytest.raises(AltEngineError) as err:
                s.with_namespace(bad).list_indexes()
            assert err.value.status == 400
            assert err.value.code == "INVALID_ARGUMENT"

    def test_list_indexes_and_delete_documents(self, s, idx):
        assert any(ix["name"] == "products" for ix in s.list_indexes()["indexes"])
        idx.put([{"id": "todelete", "fields": [{"name": "title", "type": "text", "value": "x"}]}])
        assert idx.delete(["todelete", "never-existed"]) == 1

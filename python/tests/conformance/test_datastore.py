from __future__ import annotations

import pytest

from altengine import AltEngineError

from .conftest import load_fixture, uniq


@pytest.fixture(scope="module")
def db(client):
    seed = load_fixture("datastore-seed.json")
    db = client.datastore(uniq("sdk-conf"), namespace=uniq("ns"))
    db.put(seed["collection"], seed["documents"])
    db.put("owners", seed["owners"])
    return db


class TestCRUD:
    def test_put_get_roundtrip(self, db):
        doc = db.get("todos", "t1")
        assert doc is not None
        assert doc["data"]["title"] == "ship SDK"
        assert doc["created"] > 0
        assert doc["updated"] >= doc["created"]

    def test_puts_are_upserts_and_preserve_created(self, db):
        before = db.get("todos", "t1")
        db.put("todos", [{"key": "t1", "data": {"title": "ship SDK v2", "owner": "ana", "done": False, "priority": 1}}])
        after = db.get("todos", "t1")
        assert after["data"]["title"] == "ship SDK v2"
        assert after["created"] == before["created"]
        # restore
        seed = load_fixture("datastore-seed.json")
        db.put("todos", [d for d in seed["documents"] if d["key"] == "t1"])

    def test_auto_id_and_numeric_keys(self, db):
        keys = db.put("todos", [{"data": {"title": "auto"}}, {"key": 42, "data": {"title": "num"}}])
        assert len(keys) == 2 and keys[0] and keys[1] == "42"
        assert db.get("todos", "42")["data"]["title"] == "num"
        db.delete("todos", keys)

    def test_get_list_is_order_preserving_with_nones(self, db):
        docs = db.get("todos", ["t1", "does-not-exist", "t3"])
        assert [d["key"] if d else None for d in docs] == ["t1", None, "t3"]

    def test_missing_get_none_and_delete_noop(self, db):
        assert db.get("todos", "ghost") is None
        db.delete("todos", ["ghost"])  # must not raise


class TestQuery:
    def test_filter_order_cursor_pagination(self, db):
        all_keys, cursor = [], None
        while True:
            page = db.query(
                "todos",
                where=[{"field": "done", "op": "=", "value": False}],
                order=[{"field": "priority", "dir": "desc"}],
                limit=1,
                **({"cursor": cursor} if cursor else {}),
            )
            all_keys += [d["key"] for d in page.get("documents") or []]
            cursor = page.get("cursor")
            if not cursor:
                break
        assert all_keys == ["t4", "t3", "t1"]

    def test_in_dot_paths_keys_only(self, db):
        res = db.query("todos", where=[{"field": "meta.tag", "op": "in", "value": ["home"]}], keys_only=True)
        assert sorted(res["keys"]) == ["t3", "t5"]

    def test_query_all_iterates_to_exhaustion(self, db):
        keys = [d["key"] for d in db.query_all("todos", where=[{"field": "owner", "op": "=", "value": "ana"}], limit=1)]
        assert sorted(keys) == ["t1", "t2"]

    def test_join_attaches_referenced_document(self, db):
        res = db.query(
            "todos",
            where=[{"field": "__key__", "op": "=", "value": "t1"}],
            join=[{"as": "owner_doc", "collection": "owners", "local_field": "owner"}],
        )
        assert res["documents"][0]["joins"]["owner_doc"]["data"]["name"] == "Ana"

    def test_aggregate_count_sum_grouped(self, db):
        res = db.aggregate(
            "todos",
            group=["owner"],
            metrics=[{"fn": "count", "as": "n"}, {"fn": "sum", "field": "priority", "as": "total"}],
            order=[{"field": "owner", "dir": "asc"}],
        )
        ana = next(g for g in res["groups"] if g["group"]["owner"] == "ana")
        assert ana["metrics"]["n"] == 2
        assert ana["metrics"]["total"] == 3


class TestTransactionsAndIndexes:
    def test_txn_applies_atomically(self, db):
        db.put("counters", [{"key": "c1", "data": {"total": 0}}])
        keys = db.transaction(
            [
                {"op": "check", "collection": "counters", "key": "c1", "exists": True},
                {"op": "mutate", "collection": "counters", "key": "c1", "increment": {"total": 5}},
                {"op": "put", "collection": "counters", "key": "c2", "data": {"total": 1}},
            ]
        )
        assert len(keys) == 3
        assert db.get("counters", "c1")["data"]["total"] == 5
        assert db.get("counters", "c2") is not None

    def test_failed_check_aborts_with_409(self, db):
        with pytest.raises(AltEngineError) as err:
            db.transaction(
                [
                    {"op": "check", "collection": "counters", "key": "ghost", "exists": True},
                    {"op": "mutate", "collection": "counters", "key": "c1", "increment": {"total": 100}},
                ]
            )
        assert err.value.status == 409
        assert db.get("counters", "c1")["data"]["total"] == 5

    def test_index_idempotent_and_unique_violation(self, db):
        a = db.create_index("users", ["email"], unique=True)
        b = db.create_index("users", ["email"], unique=True)
        assert b["id"] == a["id"]
        db.put("users", [{"key": "u1", "data": {"email": "x@y.z"}}])
        with pytest.raises(AltEngineError):
            db.put("users", [{"key": "u2", "data": {"email": "x@y.z"}}])
        assert any(ix["id"] == a["id"] for ix in db.list_indexes("users"))


class TestNamespaces:
    def test_isolated_listable_deletable(self, db, destructive_ok):
        other = db.with_namespace(uniq("other"))
        other.put("todos", [{"key": "only-here", "data": {"a": 1}}])
        assert db.get("todos", "only-here") is None
        assert other.get("todos", "only-here") is not None

        page = db.list_namespaces(limit=100)
        assert other.namespace in page["namespaces"]

        if destructive_ok:
            assert db.delete_namespace(other.namespace) is True
            assert other.get("todos", "only-here") is None

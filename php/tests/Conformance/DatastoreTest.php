<?php

declare(strict_types=1);

namespace AltEngine\Tests\Conformance;

use AltEngine\AltEngineError;
use AltEngine\Datastore;

final class DatastoreTest extends ConformanceTestCase
{
    private static Datastore $db;

    public static function setUpBeforeClass(): void
    {
        $seed = self::fixture('datastore-seed.json');
        self::$db = self::client()->datastore(self::uniq('sdk-conf'), self::uniq('ns'));
        self::$db->put($seed['collection'], $seed['documents']);
        self::$db->put('owners', $seed['owners']);
    }

    public function testPutGetRoundtrip(): void
    {
        $doc = self::$db->get('todos', 't1');
        $this->assertNotNull($doc);
        $this->assertSame('ship SDK', $doc['data']['title']);
        $this->assertGreaterThan(0, $doc['created']);
        $this->assertGreaterThanOrEqual($doc['created'], $doc['updated']);
    }

    public function testPutsAreUpsertsAndPreserveCreated(): void
    {
        $before = self::$db->get('todos', 't1');
        self::$db->put('todos', [
            ['key' => 't1', 'data' => ['title' => 'ship SDK v2', 'owner' => 'ana', 'done' => false, 'priority' => 1]],
        ]);
        $after = self::$db->get('todos', 't1');
        $this->assertSame('ship SDK v2', $after['data']['title']);
        $this->assertSame($before['created'], $after['created']);
        // restore
        $seed = self::fixture('datastore-seed.json');
        self::$db->put('todos', array_values(array_filter($seed['documents'], fn ($d) => $d['key'] === 't1')));
    }

    public function testAutoIdAndNumericKeys(): void
    {
        $keys = self::$db->put('todos', [['data' => ['title' => 'auto']], ['key' => 42, 'data' => ['title' => 'num']]]);
        $this->assertCount(2, $keys);
        $this->assertNotSame('', $keys[0]);
        $this->assertSame('42', $keys[1]);
        $this->assertSame('num', self::$db->get('todos', '42')['data']['title']);
        self::$db->delete('todos', $keys);
    }

    public function testGetListIsOrderPreservingWithNulls(): void
    {
        $docs = self::$db->get('todos', ['t1', 'does-not-exist', 't3']);
        $this->assertSame(['t1', null, 't3'], array_map(fn ($d) => $d['key'] ?? null, $docs));
    }

    public function testMissingGetNullAndDeleteNoop(): void
    {
        $this->assertNull(self::$db->get('todos', 'ghost'));
        self::$db->delete('todos', ['ghost']); // must not throw
    }

    public function testFilterOrderCursorPagination(): void
    {
        $all = [];
        $cursor = null;
        do {
            $req = [
                'where' => [['field' => 'done', 'op' => '=', 'value' => false]],
                'order' => [['field' => 'priority', 'dir' => 'desc']],
                'limit' => 1,
            ];
            if ($cursor !== null) {
                $req['cursor'] = $cursor;
            }
            $page = self::$db->query('todos', $req);
            foreach ($page['documents'] ?? [] as $doc) {
                $all[] = $doc['key'];
            }
            $cursor = $page['cursor'] ?? null;
        } while ($cursor);
        $this->assertSame(['t4', 't3', 't1'], $all);
    }

    public function testInDotPathsKeysOnly(): void
    {
        $res = self::$db->query('todos', [
            'where' => [['field' => 'meta.tag', 'op' => 'in', 'value' => ['home']]],
            'keys_only' => true,
        ]);
        $keys = $res['keys'];
        sort($keys);
        $this->assertSame(['t3', 't5'], $keys);
    }

    public function testQueryAllIteratesToExhaustion(): void
    {
        $keys = [];
        foreach (self::$db->queryAll('todos', [
            'where' => [['field' => 'owner', 'op' => '=', 'value' => 'ana']],
            'limit' => 1,
        ]) as $doc) {
            $keys[] = $doc['key'];
        }
        sort($keys);
        $this->assertSame(['t1', 't2'], $keys);
    }

    public function testJoinAttachesReferencedDocument(): void
    {
        $res = self::$db->query('todos', [
            'where' => [['field' => '__key__', 'op' => '=', 'value' => 't1']],
            'join' => [['as' => 'owner_doc', 'collection' => 'owners', 'local_field' => 'owner']],
        ]);
        $this->assertSame('Ana', $res['documents'][0]['joins']['owner_doc']['data']['name']);
    }

    public function testAggregateCountSumGrouped(): void
    {
        $res = self::$db->aggregate('todos', [
            'group' => ['owner'],
            'metrics' => [['fn' => 'count', 'as' => 'n'], ['fn' => 'sum', 'field' => 'priority', 'as' => 'total']],
            'order' => [['field' => 'owner', 'dir' => 'asc']],
        ]);
        $ana = null;
        foreach ($res['groups'] as $group) {
            if ($group['group']['owner'] === 'ana') {
                $ana = $group;
            }
        }
        $this->assertSame(2, $ana['metrics']['n']);
        $this->assertSame(3, $ana['metrics']['total']);
    }

    public function testTransactionAppliesAtomically(): void
    {
        self::$db->put('counters', [['key' => 'c1', 'data' => ['total' => 0]]]);
        $keys = self::$db->transaction([
            ['op' => 'check', 'collection' => 'counters', 'key' => 'c1', 'exists' => true],
            ['op' => 'mutate', 'collection' => 'counters', 'key' => 'c1', 'increment' => ['total' => 5]],
            ['op' => 'put', 'collection' => 'counters', 'key' => 'c2', 'data' => ['total' => 1]],
        ]);
        $this->assertCount(3, $keys);
        $this->assertSame(5, self::$db->get('counters', 'c1')['data']['total']);
        $this->assertNotNull(self::$db->get('counters', 'c2'));
    }

    public function testFailedCheckAbortsWith409(): void
    {
        try {
            self::$db->transaction([
                ['op' => 'check', 'collection' => 'counters', 'key' => 'ghost', 'exists' => true],
                ['op' => 'mutate', 'collection' => 'counters', 'key' => 'c1', 'increment' => ['total' => 100]],
            ]);
            $this->fail('transaction should have thrown');
        } catch (AltEngineError $err) {
            $this->assertSame(409, $err->status);
        }
        $this->assertSame(5, self::$db->get('counters', 'c1')['data']['total']);
    }

    public function testIndexIdempotentAndUniqueViolation(): void
    {
        $a = self::$db->createIndex('users', ['email'], unique: true);
        $b = self::$db->createIndex('users', ['email'], unique: true);
        $this->assertSame($a['id'], $b['id']);
        self::$db->put('users', [['key' => 'u1', 'data' => ['email' => 'x@y.z']]]);
        try {
            self::$db->put('users', [['key' => 'u2', 'data' => ['email' => 'x@y.z']]]);
            $this->fail('unique violation should have thrown');
        } catch (AltEngineError $err) {
            $this->assertNotSame(200, $err->status);
        }
        $ids = array_column(self::$db->listIndexes('users'), 'id');
        $this->assertContains($a['id'], $ids);
    }

    public function testNamespacesIsolatedListableDeletable(): void
    {
        $other = self::$db->withNamespace(self::uniq('other'));
        $other->put('todos', [['key' => 'only-here', 'data' => ['a' => 1]]]);
        $this->assertNull(self::$db->get('todos', 'only-here'));
        $this->assertNotNull($other->get('todos', 'only-here'));

        $page = self::$db->listNamespaces(limit: 100);
        $this->assertContains($other->namespace, $page['namespaces']);

        if (self::destructiveOk()) {
            $this->assertTrue(self::$db->deleteNamespace($other->namespace));
            $this->assertNull($other->get('todos', 'only-here'));
        }
    }
}

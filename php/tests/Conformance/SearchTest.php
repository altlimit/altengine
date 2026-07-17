<?php

declare(strict_types=1);

namespace AltEngine\Tests\Conformance;

use AltEngine\AltEngineError;
use AltEngine\Search;
use AltEngine\SearchIndex;

final class SearchTest extends ConformanceTestCase
{
    private static Search $s;
    private static SearchIndex $idx;

    public static function setUpBeforeClass(): void
    {
        $corpus = self::fixture('search-corpus.json');
        self::$s = self::client()->search(self::uniq('sdk-conf'));
        self::$idx = self::$s->index('products');
        $ids = self::$idx->put($corpus['documents']);
        if ($ids !== array_column($corpus['documents'], 'id')) {
            self::fail('put ids do not echo the corpus ids');
        }
    }

    /** @return list<string> */
    private static function hitIds(array $res): array
    {
        return array_column($res['results'], 'id');
    }

    public function testGetRoundtripsAllFieldTypes(): void
    {
        $doc = self::$idx->get('p1');
        $this->assertNotNull($doc);
        $byName = array_column($doc['fields'], null, 'name');
        $this->assertSame('Blue Suede Shoes', $byName['title']['value']);
        $this->assertSame(59, $byName['price']['value']);
        $this->assertSame(37.77, $byName['store']['value']['lat']);
        $this->assertCount(2, $doc['facets']);
    }

    public function testGetMissingReturnsNull(): void
    {
        $this->assertNull(self::$idx->get('nope'));
    }

    public function testGetListIsOrderPreservingWithNulls(): void
    {
        $docs = self::$idx->get(['p1', 'does-not-exist', 'p3']);
        $this->assertSame(['p1', null, 'p3'], array_map(fn ($d) => $d['id'] ?? null, $docs));
    }

    public function testServerAssignsIds(): void
    {
        $ids = self::$idx->put([['fields' => [['name' => 'title', 'type' => 'text', 'value' => 'temp']]]]);
        $this->assertNotSame('', $ids[0]);
        self::$idx->delete($ids);
    }

    public function testListAllDocumentsKeyset(): void
    {
        $seen = [];
        foreach (self::$idx->listAllDocuments(limit: 2) as $doc) {
            $seen[] = $doc['id'];
        }
        sort($seen);
        $this->assertSame(['p1', 'p2', 'p3', 'p4'], $seen);
    }

    public function testBareAndFieldTerms(): void
    {
        $ids = self::hitIds(self::$idx->search('shoes'));
        sort($ids);
        $this->assertSame(['p1', 'p2', 'p4'], $ids);
        $this->assertSame(['p1'], self::hitIds(self::$idx->search('sku:"BS-001"')));
    }

    public function testBooleanOperatorsAndComparisons(): void
    {
        $ids = self::hitIds(self::$idx->search('shoes AND price<100'));
        sort($ids);
        $this->assertSame(['p1', 'p4'], $ids);
        $this->assertSame(['p2'], self::hitIds(self::$idx->search('shoes NOT blue')));
    }

    public function testSortAndCursorPagination(): void
    {
        $page1 = self::$idx->search('shoes', ['sort' => [['expr' => 'price']], 'limit' => 2]);
        $this->assertSame(['p4', 'p1'], self::hitIds($page1));
        $this->assertNotEmpty($page1['cursor']);
        $page2 = self::$idx->search('shoes', ['sort' => [['expr' => 'price']], 'limit' => 2, 'cursor' => $page1['cursor']]);
        $this->assertSame(['p2'], self::hitIds($page2));
    }

    public function testFacetsAndRefinements(): void
    {
        $res = self::$idx->search('', ['facets' => ['category']]);
        $cat = array_column($res['facets'], null, 'name')['category'];
        $shoes = array_column($cat['values'], null, 'value')['shoes'];
        $this->assertSame(3, $shoes['count']);

        $refined = self::$idx->search('', ['facet_refinements' => [['name' => 'category', 'value' => 'accessories']]]);
        $this->assertSame(['p3'], self::hitIds($refined));
    }

    public function testSnippets(): void
    {
        $res = self::$idx->search('suede', ['snippet' => ['fields' => ['title'], 'pre_tag' => '<em>', 'post_tag' => '</em>']]);
        $this->assertStringContainsString('<em>', $res['results'][0]['snippet']['title']);
    }

    public function testCollapse(): void
    {
        $res = self::$idx->search('shoes', ['collapse' => ['field' => 'category', 'limit' => 1]]);
        $this->assertCount(1, $res['results']);
    }

    public function testIdsOnly(): void
    {
        $res = self::$idx->search('shoes', ['ids_only' => true]);
        foreach ($res['results'] as $hit) {
            $this->assertArrayNotHasKey('document', $hit);
        }
    }

    public function testSearchAllPagesCursors(): void
    {
        $this->assertCount(3, iterator_to_array(self::$idx->searchAll('shoes', ['limit' => 1]), false));
    }

    public function testSchemaUnion(): void
    {
        $schema = self::$idx->schema();
        $this->assertSame('products', $schema['name']);
        $this->assertContains('number', $schema['fields']['price']);
    }

    public function testNamespaceIsolationAndListing(): void
    {
        $other = self::$s->withNamespace(self::uniq('ns'));
        $other->index('products')->put([['id' => 'only', 'fields' => [['name' => 'title', 'type' => 'text', 'value' => 'hidden']]]]);
        $this->assertNull(self::$idx->get('only'));
        $this->assertContains('products', array_column($other->listIndexes()['indexes'], 'name'));

        $page = self::$s->listNamespaces(limit: 100);
        $this->assertContains($other->namespace, $page['namespaces']);
        $this->assertContains('', $page['namespaces']);

        if (self::destructiveOk()) {
            $this->assertTrue($other->deleteIndex('products'));
        }
    }

    public function testInvalidNamespacesRejected(): void
    {
        foreach (["a\x00b", str_repeat('x', 101), "caf\u{e9}"] as $bad) {
            try {
                self::$s->withNamespace($bad)->listIndexes();
                $this->fail('invalid namespace should have thrown: ' . bin2hex($bad));
            } catch (AltEngineError $err) {
                $this->assertSame(400, $err->status);
                $this->assertSame('INVALID_ARGUMENT', $err->errorCode);
            }
        }
    }

    public function testListIndexesAndDeleteDocuments(): void
    {
        $this->assertContains('products', array_column(self::$s->listIndexes()['indexes'], 'name'));
        self::$idx->put([['id' => 'todelete', 'fields' => [['name' => 'title', 'type' => 'text', 'value' => 'x']]]]);
        $this->assertSame(1, self::$idx->delete(['todelete', 'never-existed']));
    }
}

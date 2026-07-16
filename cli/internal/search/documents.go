package search

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
)

// Field is one field of a document.
type Field struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Value    json.RawMessage `json:"value"`
	Language string          `json:"language,omitempty"`
}

// Facet is one facet value (separate namespace from fields).
type Facet struct {
	Name  string          `json:"name"`
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// Document is the AltDocument wire shape.
type Document struct {
	ID     string  `json:"id"`
	Rank   *int64  `json:"rank,omitempty"`
	Lang   string  `json:"lang,omitempty"`
	Fields []Field `json:"fields"`
	Facets []Facet `json:"facets,omitempty"`
}

var tokenizedTypes = map[string]bool{"text": true, "html": true, "tokenprefix": true}
var validFieldTypes = map[string]bool{
	"text": true, "html": true, "atom": true, "number": true,
	"date": true, "geo": true, "tokenprefix": true, "untokenprefix": true,
}

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

func stripHTML(s string) string {
	return strings.Join(strings.Fields(htmlTagRe.ReplaceAllString(s, " ")), " ")
}

// parseDate accepts ISO-8601, YYYY-MM-DD, or epoch-ms and returns epoch ms.
func parseDate(raw json.RawMessage) (int64, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UnixMilli(), nil
		}
		if t, err := time.Parse("2006-01-02", s); err == nil {
			return t.UnixMilli(), nil
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, nil
		}
		return 0, common.BadRequest("invalid date value")
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return int64(n), nil
	}
	return 0, common.BadRequest("invalid date value")
}

type geoVal struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

func defaultRank() int64 { return time.Now().Unix() - rankEpoch }

// Put inserts or replaces documents in an index, returning generated ids.
func (s *Store) Put(indexName string, docs []Document) ([]string, error) {
	if len(docs) > maxDocsPerBatch {
		return nil, common.BadRequest(fmt.Sprintf("too many documents (max %d)", maxDocsPerBatch))
	}
	ix, err := s.ensureIndex(indexName)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		id := d.ID
		if id == "" {
			id = common.Base62(16)
		} else if !docIDRe.MatchString(id) || strings.HasPrefix(id, "!") || reservedRe.MatchString(id) {
			return nil, common.BadRequest("invalid document id")
		}
		rank := defaultRank()
		if d.Rank != nil {
			if *d.Rank < 0 || *d.Rank >= (1<<31) {
				return nil, common.BadRequest("rank out of range")
			}
			rank = *d.Rank
		}
		if err := ix.replaceDoc(tx, id, rank, d); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

func (ix *Index) replaceDoc(tx *sql.Tx, id string, rank int64, d Document) error {
	p := ix.prefix
	// Remove any existing rows for this doc (insert-or-replace).
	for _, t := range []string{"_docs", "_fields", "_fts", "_facets"} {
		if _, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s%s WHERE doc_id=?`, p, t), id); err != nil {
			return err
		}
	}
	if ix.stemming {
		tx.Exec(fmt.Sprintf(`DELETE FROM %s_fts_stem WHERE doc_id=?`, p), id)
	}
	body, _ := json.Marshal(d)
	if _, err := tx.Exec(fmt.Sprintf(`INSERT INTO %s_docs(doc_id, rank, lang, body) VALUES(?,?,?,?)`, p),
		id, rank, d.Lang, string(body)); err != nil {
		return err
	}
	for _, f := range d.Fields {
		if !fieldNameRe.MatchString(f.Name) {
			return common.BadRequest("invalid field name: " + f.Name)
		}
		if !validFieldTypes[f.Type] {
			return common.BadRequest("invalid field type: " + f.Type)
		}
		if err := ix.indexField(tx, id, f); err != nil {
			return err
		}
		tx.Exec(fmt.Sprintf(`INSERT OR IGNORE INTO %s_schema(name, type) VALUES(?,?)`, p), f.Name, f.Type)
	}
	for _, fc := range d.Facets {
		if err := ix.indexFacet(tx, id, fc); err != nil {
			return err
		}
	}
	return nil
}

func (ix *Index) indexField(tx *sql.Tx, id string, f Field) error {
	p := ix.prefix
	switch f.Type {
	case "text", "html", "tokenprefix":
		var s string
		json.Unmarshal(f.Value, &s)
		if f.Type == "html" {
			s = stripHTML(s)
		}
		if _, err := tx.Exec(fmt.Sprintf(`INSERT INTO %s_fts(content, doc_id, name) VALUES(?,?,?)`, p), s, id, f.Name); err != nil {
			return err
		}
		if ix.stemming {
			tx.Exec(fmt.Sprintf(`INSERT INTO %s_fts_stem(content, doc_id, name) VALUES(?,?,?)`, p), s, id, f.Name)
		}
	case "atom":
		var s string
		json.Unmarshal(f.Value, &s)
		if len(s) > maxAtomBytes {
			return common.BadRequest("atom value too long")
		}
		_, err := tx.Exec(fmt.Sprintf(`INSERT INTO %s_fields(doc_id,name,type,text_lc) VALUES(?,?,?,?)`, p),
			id, f.Name, f.Type, strings.ToLower(s))
		return err
	case "untokenprefix":
		var s string
		json.Unmarshal(f.Value, &s)
		_, err := tx.Exec(fmt.Sprintf(`INSERT INTO %s_fields(doc_id,name,type,text_lc) VALUES(?,?,?,?)`, p),
			id, f.Name, f.Type, strings.ToLower(s))
		return err
	case "number":
		var n float64
		if json.Unmarshal(f.Value, &n) != nil {
			return common.BadRequest("invalid number value")
		}
		_, err := tx.Exec(fmt.Sprintf(`INSERT INTO %s_fields(doc_id,name,type,num_val) VALUES(?,?,?,?)`, p),
			id, f.Name, f.Type, n)
		return err
	case "date":
		ms, err := parseDate(f.Value)
		if err != nil {
			return err
		}
		_, err = tx.Exec(fmt.Sprintf(`INSERT INTO %s_fields(doc_id,name,type,num_val) VALUES(?,?,?,?)`, p),
			id, f.Name, f.Type, float64(ms))
		return err
	case "geo":
		var g geoVal
		if json.Unmarshal(f.Value, &g) != nil {
			return common.BadRequest("invalid geo value")
		}
		if g.Lat < -90 || g.Lat > 90 || g.Lng < -180 || g.Lng > 180 {
			return common.BadRequest("geo out of range")
		}
		_, err := tx.Exec(fmt.Sprintf(`INSERT INTO %s_fields(doc_id,name,type,lat,lng) VALUES(?,?,?,?,?)`, p),
			id, f.Name, f.Type, g.Lat, g.Lng)
		return err
	}
	return nil
}

func (ix *Index) indexFacet(tx *sql.Tx, id string, fc Facet) error {
	p := ix.prefix
	switch fc.Type {
	case "atom", "":
		var s string
		json.Unmarshal(fc.Value, &s)
		_, err := tx.Exec(fmt.Sprintf(`INSERT INTO %s_facets(doc_id,name,kind,text_val) VALUES(?,?,1,?)`, p), id, fc.Name, s)
		return err
	case "number":
		var n float64
		json.Unmarshal(fc.Value, &n)
		_, err := tx.Exec(fmt.Sprintf(`INSERT INTO %s_facets(doc_id,name,kind,num_val) VALUES(?,?,2,?)`, p), id, fc.Name, n)
		return err
	}
	return nil
}

// Get returns one document.
func (s *Store) Get(indexName, docID string) (*Document, error) {
	ix, err := s.getIndex(indexName)
	if err != nil {
		return nil, err
	}
	if ix == nil {
		return nil, common.NotFound("index not found")
	}
	var body string
	err = s.db.QueryRow(fmt.Sprintf(`SELECT body FROM %s_docs WHERE doc_id=?`, ix.prefix), docID).Scan(&body)
	if err == sql.ErrNoRows {
		return nil, common.NotFound("document not found")
	}
	if err != nil {
		return nil, err
	}
	var d Document
	json.Unmarshal([]byte(body), &d)
	return &d, nil
}

// List returns documents ordered by doc_id, optionally starting at start_id.
func (s *Store) List(indexName, startID string, includeStart bool, limit int, idsOnly bool) ([]Document, []string, error) {
	ix, err := s.getIndex(indexName)
	if err != nil {
		return nil, nil, err
	}
	if ix == nil {
		return nil, nil, common.NotFound("index not found")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	cmp := ">="
	if !includeStart {
		cmp = ">"
	}
	q := fmt.Sprintf(`SELECT doc_id, body FROM %s_docs`, ix.prefix)
	var args []any
	if startID != "" {
		q += " WHERE doc_id " + cmp + " ?"
		args = append(args, startID)
	}
	q += " ORDER BY doc_id ASC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var docs []Document
	var ids []string
	for rows.Next() {
		var did, body string
		if err := rows.Scan(&did, &body); err != nil {
			return nil, nil, err
		}
		ids = append(ids, did)
		if !idsOnly {
			var d Document
			json.Unmarshal([]byte(body), &d)
			docs = append(docs, d)
		}
	}
	return docs, ids, nil
}

// DeleteDocs removes documents by id, returning the count removed.
func (s *Store) DeleteDocs(indexName string, ids []string) (int, error) {
	if len(ids) > maxIDsPerDelete {
		return 0, common.BadRequest(fmt.Sprintf("too many ids (max %d)", maxIDsPerDelete))
	}
	ix, err := s.getIndex(indexName)
	if err != nil {
		return 0, err
	}
	if ix == nil {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	total := 0
	for _, id := range ids {
		res, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s_docs WHERE doc_id=?`, ix.prefix), id)
		if err != nil {
			return 0, err
		}
		for _, t := range []string{"_fields", "_fts", "_facets"} {
			tx.Exec(fmt.Sprintf(`DELETE FROM %s%s WHERE doc_id=?`, ix.prefix, t), id)
		}
		if ix.stemming {
			tx.Exec(fmt.Sprintf(`DELETE FROM %s_fts_stem WHERE doc_id=?`, ix.prefix), id)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

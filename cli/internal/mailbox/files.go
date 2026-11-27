package mailbox

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/altlimit/altengine/cli/internal/blob"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// A mailbox's files — attachments, and the raw .eml when keepRaw is on — live in a BLOB INSTANCE
// the mailbox is pointed at, its file store, as hosted (src/mailbox/files.ts). Each is an ordinary
// private object there, named <mailbox>-<message>…, and a message row holds its key
// `blob/<blob-instance-id>/<blob-id>`. With no file store, attachments are listed and not stored.
//
// Keys of any other shape are files this emulator wrote beside the mailbox's own database before
// file stores existed; they are still read and deleted where they are.

// BlobFiles is the blob service's object store, as the mailbox writes into it.
type BlobFiles interface {
	Reserve(instanceID string, cfg blob.Config, req blob.MintRequest) (*blob.Record, error)
	Commit(instanceID, id string, body []byte) (*blob.Record, error)
	Bytes(instanceID, id string) ([]byte, bool)
	Remove(instanceID string, ids []string) []*blob.Record
}

// WithFiles gives the service the blob store its file stores live in.
func (s *Service) WithFiles(f BlobFiles) *Service { s.store.files = f; return s }

// blobKey is the key a file in a store is recorded under.
func blobKey(storeID, id string) string { return "blob/" + storeID + "/" + id }

// parseBlobKey splits `blob/<store>/<id>`, or reports that the key is a legacy local one.
func parseBlobKey(key string) (storeID, id string, ok bool) {
	p := strings.Split(key, "/")
	if len(p) != 3 || p[0] != "blob" || p[1] == "" || p[2] == "" {
		return "", "", false
	}
	return p[1], p[2], true
}

// BlobIDOf is a stored file's object id in its store, or nil for none / a legacy file.
func BlobIDOf(key string) *string {
	if _, id, ok := parseBlobKey(key); ok {
		return &id
	}
	return nil
}

// fileStore is where one message's files go, resolved when it arrives.
type fileStore struct {
	id  string
	cfg blob.Config
}

// resolveFileStore is the mailbox's store, or nil to keep none — nil too when the blob instance
// it names is gone.
func (s *Service) resolveFileStore(inst *control.Instance) *fileStore {
	if s.store.files == nil {
		return nil
	}
	id, err := s.store.FileStore(inst.ID)
	if err != nil || id == "" {
		return nil
	}
	b := s.reg.GetByID("blob", id)
	if b == nil {
		return nil
	}
	return &fileStore{id: b.ID, cfg: blob.ParseConfig(b.Config())}
}

// FileName is a file's object name: one segment, led by the mailbox and message so a prefix
// listing in the blob console finds a message's files.
func FileName(mailbox, messageID string, idx int, filename *string) string {
	if idx < 0 {
		return mailbox + "-" + messageID + ".eml"
	}
	f := "attachment"
	if filename != nil && *filename != "" {
		f = *filename
	}
	n := fmt.Sprintf("%s-%s-%d-%s", mailbox, messageID, idx, f)
	if len(n) > 500 {
		n = n[:500]
	}
	return n
}

// putFile writes one private object into the store and returns its key.
func (s *Service) putFile(fs *fileStore, name, contentType string, body []byte, meta map[string]any) (string, error) {
	size := int64(len(body))
	private := false
	clean := strings.NewReplacer("/", "-", "\\", "-", "\r", "-", "\n", "-").Replace(name)
	rec, err := s.store.files.Reserve(fs.id, fs.cfg, blob.MintRequest{Name: clean, Size: &size, ContentType: contentType, Public: &private, Meta: meta})
	if err != nil {
		return "", err
	}
	if _, err := s.store.files.Commit(fs.id, rec.ID, body); err != nil {
		s.store.files.Remove(fs.id, []string{rec.ID})
		return "", err
	}
	return blobKey(fs.id, rec.ID), nil
}

// removeFiles deletes files by key, either shape: a store's object goes with its row, so blob
// stops counting it.
func (st *Store) removeFiles(instanceID string, keys []string) {
	var local []string
	byStore := map[string][]string{}
	for _, k := range keys {
		if k == "" {
			continue
		}
		if sid, id, ok := parseBlobKey(k); ok {
			byStore[sid] = append(byStore[sid], id)
		} else {
			local = append(local, k)
		}
	}
	if st.files != nil {
		for sid, ids := range byStore {
			st.files.Remove(sid, ids)
		}
	}
	st.deleteBytes(instanceID, local)
}

// --- the file-store pointer --------------------------------------------------------------------
//
// Kept in the instance's own database rather than its config, as hosted keeps it out of the
// config blob: a full config replace must not unlink it.

// FileStore is the blob instance id this mailbox keeps files in, or "".
func (st *Store) FileStore(instanceID string) (string, error) {
	db, err := st.db(instanceID)
	if err != nil {
		return "", err
	}
	var v string
	err = db.QueryRow(`SELECT v FROM meta WHERE k = 'file_store'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetFileStore points the mailbox at a blob instance id, or at none with "".
func (st *Store) SetFileStore(instanceID, storeID string) error {
	db, err := st.db(instanceID)
	if err != nil {
		return err
	}
	if storeID == "" {
		_, err = db.Exec(`DELETE FROM meta WHERE k = 'file_store'`)
		return err
	}
	_, err = db.Exec(`INSERT INTO meta (k, v) VALUES ('file_store', ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, storeID)
	return err
}

// releaseFiles removes every store-held file the instance's messages name — the instance delete,
// before its database goes.
func (st *Store) releaseFiles(instanceID string) {
	db, err := st.db(instanceID)
	if err != nil {
		return
	}
	var keys []string
	for _, q := range []string{
		`SELECT raw_key FROM message WHERE raw_key LIKE 'blob/%'`,
		`SELECT file_key FROM attachment WHERE file_key LIKE 'blob/%'`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			continue
		}
		for rows.Next() {
			var k string
			if rows.Scan(&k) == nil {
				keys = append(keys, k)
			}
		}
		rows.Close()
	}
	st.removeFiles(instanceID, keys)
}

// SetFileStore points a mailbox at a blob instance (by name, in this emulator's one org) or at
// none (""). Returns the store's name, or "" for none. The one implementation behind the console
// and MCP.
func (s *Service) SetFileStore(inst *control.Instance, blobName string) (*control.Instance, error) {
	if blobName == "" {
		return nil, s.store.SetFileStore(inst.ID, "")
	}
	b := s.reg.Get("blob", blobName)
	if b == nil {
		return nil, common.NotFound(fmt.Sprintf("blob instance '%s' not found", blobName))
	}
	return b, s.store.SetFileStore(inst.ID, b.ID)
}

// FileStoreInstance is the blob instance a mailbox keeps files in, or nil.
func (s *Service) FileStoreInstance(inst *control.Instance) *control.Instance {
	id, err := s.store.FileStore(inst.ID)
	if err != nil || id == "" {
		return nil
	}
	return s.reg.GetByID("blob", id)
}

// GuardBlobDelete refuses deleting a blob instance a mailbox keeps its files in: those message
// rows are the only handle on the files. Registered with the registry's OnBeforeDelete.
func (s *Service) GuardBlobDelete(blobID string) error {
	var using []string
	for _, m := range s.reg.List("mailbox") {
		if id, err := s.store.FileStore(m.ID); err == nil && id == blobID {
			using = append(using, "'"+m.Name+"'")
		}
	}
	if len(using) == 0 {
		return nil
	}
	plural := len(using) > 1
	what, it, del := "mailbox", "it", "mailbox"
	if plural {
		what, it, del = "mailboxes", "them", "mailboxes"
	}
	return common.NewError(409, fmt.Sprintf(
		"this blob instance keeps the files of %s %s — point %s at another file store or none, or delete the %s first",
		what, strings.Join(using, ", "), it, del), "FAILED_PRECONDITION")
}

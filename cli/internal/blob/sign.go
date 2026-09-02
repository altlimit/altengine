// Signing the URLs that carry bytes.
//
// Hosted, these are SigV4 presigned URLs against object storage. Reproducing SigV4 here would
// emulate the wrong thing: no local client parses the signature, and a faithful copy of the
// algorithm would still not be checked by anything real. What matters — and what this does
// reproduce — is the PROPERTY the presigned URL has: it is a bearer capability, scoped to one
// method on one object, that stops working after a few minutes.
//
// The key is random per process. A URL minted by one `altengine dev` run is worthless to the
// next, which is the correct local answer: the capability is not a credential to be stored.
package blob

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
)

type signer struct{ key []byte }

func newSigner() *signer {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return &signer{key: k}
}

func (s *signer) mac(method, instanceID, id string, exp int64, query string) string {
	h := hmac.New(sha256.New, s.key)
	fmt.Fprintf(h, "%s\n%s\n%s\n%d\n%s", method, instanceID, id, exp, query)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// canonicalQuery is the part of a query string that is covered by the signature: everything
// except the two parameters the signature itself is carried in.
//
// Multipart's whole vocabulary lives in query parameters — `uploads`, `uploadId`, `partNumber` —
// so a signature that ignored them would let whoever holds one part URL write any part of any
// upload for that object. Sorted, so the string does not depend on map iteration order.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		if k == "exp" || k == "sig" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+q.Get(k))
	}
	return strings.Join(parts, "&")
}

// query returns the signed query string for a capability that expires after ttl. `extra` carries
// the multipart vocabulary, which is folded into both the URL and the signature.
func (s *signer) query(method, instanceID, id string, ttl time.Duration, extra url.Values) (string, int64) {
	exp := time.Now().Add(ttl).UnixMilli()
	q := url.Values{}
	for k, v := range extra {
		q[k] = v
	}
	sig := s.mac(method, instanceID, id, exp, canonicalQuery(q))
	q.Set("exp", strconv.FormatInt(exp, 10))
	q.Set("sig", sig)
	return q.Encode(), exp
}

// verify checks a presented capability. Expiry is checked first so an expired URL says so
// rather than reading as a bad signature — the two are indistinguishable to a caller otherwise,
// and the fix for each is different.
func (s *signer) verify(method, instanceID, id string, q url.Values) error {
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil {
		return common.Unauthenticated("this URL is missing its signature")
	}
	if time.Now().UnixMilli() > exp {
		return common.Unauthenticated("this URL has expired — ask for a new one")
	}
	if !hmac.Equal([]byte(q.Get("sig")), []byte(s.mac(method, instanceID, id, exp, canonicalQuery(q)))) {
		return common.Unauthenticated("this URL's signature is not valid")
	}
	return nil
}

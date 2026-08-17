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
	"strconv"
	"time"

	"github.com/altlimit/altengine/cli/internal/common"
)

type signer struct{ key []byte }

func newSigner() *signer {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return &signer{key: k}
}

func (s *signer) mac(method, instanceID, id string, exp int64) string {
	h := hmac.New(sha256.New, s.key)
	fmt.Fprintf(h, "%s\n%s\n%s\n%d", method, instanceID, id, exp)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// query returns the signed query string for a capability that expires after ttl.
func (s *signer) query(method, instanceID, id string, ttl time.Duration) (string, int64) {
	exp := time.Now().Add(ttl).UnixMilli()
	q := url.Values{"exp": {strconv.FormatInt(exp, 10)}, "sig": {s.mac(method, instanceID, id, exp)}}
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
	if !hmac.Equal([]byte(q.Get("sig")), []byte(s.mac(method, instanceID, id, exp))) {
		return common.Unauthenticated("this URL's signature is not valid")
	}
	return nil
}

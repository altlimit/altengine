// Package mailbox emulates the altengine mailbox service: email addresses an app or an agent
// owns. Mail arrives through the same admission and ingest path the hosted service runs —
// from the /v1 inject endpoint, the console's compose form, a raw .eml upload, or the optional
// SMTP listener — and is stored, matched against the instance's routes, and handed to a
// function, a channel or a webhook. Nothing sent from here leaves the emulator.
package mailbox

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/altlimit/altengine/cli/internal/common"
)

// THE SHAPE IS `<slug>[.<name>][+<tag>]@<mail domain>`, all on ONE domain:
//
//	acme@mail.localhost              the instance's main address (name `inbox`)
//	acme.signup-7f3a@mail.localhost  the same instance, name `signup-7f3a`
//	acme.support+42@mail.localhost   name `support`, tag `42`
//
// A dot separates slug from name because a slug cannot contain one, so the first dot is
// unambiguous. Hosted, the slug is minted per instance; locally an instance's slug is its NAME
// (the same stand-in the other public-address services use), so an instance whose name is not a
// valid slug has no address and is reached only through inject and the console.

// DefaultLocal is the name a bare `<slug>@` address files under.
const DefaultLocal = "inbox"

// MaxLocalPart is RFC 5321's limit on the whole local part — slug, dot and name together.
const MaxLocalPart = 64

// DefaultDomain is the emulator's mail domain when none is configured.
const DefaultDomain = "mail.localhost"

var (
	localRe = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)
	tagRe   = regexp.MustCompile(`^[\x21-\x7e]{1,64}$`)
	// The hosted slug rule: 3–32 characters, lowercase letters, digits and inner hyphens.
	slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)
	outRe  = regexp.MustCompile(`(?i)^[^\s@<>()",;:\\\[\]]{1,64}@[a-z0-9-]+(\.[a-z0-9-]+)+$`)
)

// NormalizeDomain trims, lowercases and strips the leading/trailing dot of a configured domain.
func NormalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimPrefix(d, ".")
	return strings.TrimSuffix(d, ".")
}

// ValidSlug reports whether s can be the first part of an address.
func ValidSlug(s string) bool { return slugRe.MatchString(s) && !strings.Contains(s, "--") }

// SlugOf is an instance's slug locally: its name, when that is a valid slug, else "".
func SlugOf(instanceName string) string {
	if ValidSlug(instanceName) {
		return instanceName
	}
	return ""
}

// AddressFor is the address for a name on an instance. The default name is the bare `<slug>@`.
func AddressFor(slug, local, domain string) string {
	if local == DefaultLocal {
		return slug + "@" + domain
	}
	return slug + "." + local + "@" + domain
}

// AddressPattern is how every address of an instance looks, for showing a person.
func AddressPattern(slug, domain string) string { return slug + ".<anything>@" + domain }

// Local is a name with its optional plus tag.
type Local struct {
	Local string
	Tag   string // "" = none
}

// ParseLocal splits `name+tag` and normalizes. ok is false when the name is not one we accept.
// A tag that is not printable is dropped rather than refused: it is only a hint the sender added.
func ParseLocal(raw string) (Local, bool) {
	lower := strings.ToLower(strings.TrimSpace(raw))
	local, tag := lower, ""
	if i := strings.Index(lower, "+"); i >= 0 {
		local, tag = lower[:i], lower[i+1:]
	}
	if !localRe.MatchString(local) {
		return Local{}, false
	}
	if tag != "" && !tagRe.MatchString(tag) {
		tag = ""
	}
	return Local{Local: local, Tag: tag}, true
}

// ValidateLocal checks a name a caller chose through the API, for an instance whose slug is
// `slug` ("" skips the length check against it): no plus tag, and short enough that the whole
// local part fits RFC 5321's 64 characters.
func ValidateLocal(raw any, slug string) (string, error) {
	s, ok := raw.(string)
	if !ok {
		return "", common.BadRequest("local must be a string")
	}
	local := strings.ToLower(strings.TrimSpace(s))
	if !localRe.MatchString(local) {
		return "", common.BadRequest("local must be 1–64 characters of lowercase letters, digits, '.', '_' or '-' (no '+')")
	}
	if slug != "" && local != DefaultLocal && len(slug)+1+len(local) > MaxLocalPart {
		return "", common.BadRequest(fmt.Sprintf("local is too long for this mailbox: '%s.' plus the name may be at most %d characters", slug, MaxLocalPart))
	}
	return local, nil
}

// MintLocal is a name for a caller who did not choose one: `m` + 11 hex.
func MintLocal() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "m" + hex.EncodeToString(b)[:11]
}

// Recipient is an envelope recipient resolved to the instance slug and name it addresses.
type Recipient struct {
	Slug  string
	Local string
	Tag   string
}

// ParseRecipient resolves an address to the instance and name it addresses, or ok=false when it
// is not a mailbox address on this mail domain.
func ParseRecipient(address, domain string) (Recipient, bool) {
	if domain == "" {
		return Recipient{}, false
	}
	a := strings.TrimSpace(address)
	at := strings.LastIndex(a, "@")
	if at < 1 || at == len(a)-1 {
		return Recipient{}, false
	}
	if strings.TrimSuffix(strings.ToLower(a[at+1:]), ".") != domain {
		return Recipient{}, false
	}
	localPart := strings.ToLower(a[:at])
	if len(localPart) > MaxLocalPart {
		return Recipient{}, false
	}
	base, tail := localPart, ""
	if i := strings.Index(localPart, "+"); i >= 0 {
		base, tail = localPart[:i], localPart[i:]
	}
	slug, name := base, DefaultLocal
	if i := strings.Index(base, "."); i >= 0 {
		slug, name = base[:i], base[i+1:]
	}
	if !ValidSlug(slug) {
		return Recipient{}, false
	}
	p, ok := ParseLocal(name + tail)
	if !ok {
		return Recipient{}, false
	}
	return Recipient{Slug: slug, Local: p.Local, Tag: p.Tag}, true
}

// NameFilter reads a name filter from a caller: a bare name (`support`) or a full address on
// THIS instance. "" for an empty filter; an error for anything else, including an address on
// another instance.
func NameFilter(raw, slug, domain string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	bad := common.BadRequest("to must be a name (e.g. 'support') or a full address on this mailbox")
	if strings.Contains(v, "@") {
		r, ok := ParseRecipient(v, domain)
		if !ok || slug == "" || r.Slug != slug {
			return "", bad
		}
		return r.Local, nil
	}
	p, ok := ParseLocal(v)
	if !ok {
		return "", bad
	}
	return p.Local, nil
}

// ValidateOutboundAddress is a bare `user@domain` check for addresses we SEND to: one `@`, no
// whitespace, nothing that could end a header or open another address, a dotted domain.
func ValidateOutboundAddress(raw any, field string) (string, error) {
	s, ok := raw.(string)
	if !ok || len(s) > 254 || !outRe.MatchString(strings.TrimSpace(s)) {
		return "", common.BadRequest(field + " must be a plain email address (user@example.com)")
	}
	return strings.TrimSpace(s), nil
}

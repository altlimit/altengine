package mailbox

import (
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/altlimit/altengine/cli/internal/control"
)

// Inbound mail. THE ORDER IS THE POINT, as hosted: everything that can refuse a message refuses
// it BEFORE the body is read — an address that is not ours, an instance that does not exist, an
// oversize message, a disabled or unregistered address, a mailbox over its day's cap. Each is an
// SMTP answer to the sender, never a stored record. Only then is the body parsed and stored.
//
// Every way mail gets in — /v1 inject, the console's compose form and .eml upload, the SMTP
// listener, a send looped back to a local address — goes through Admit* and Ingest below, so
// the path a test exercises is the path the hosted service runs.

const (
	// MaxMessageBytes is the largest message accepted, total.
	MaxMessageBytes = 25 * 1024 * 1024
	// MaxAttachmentBytes is the largest single attachment stored. A bigger part is dropped and
	// listed as rejected; the message is kept.
	MaxAttachmentBytes = 10 * 1024 * 1024
	// MaxAttachments is attachments stored per message; past it they are listed as rejected.
	MaxAttachments = 100
)

// Rejection is a refusal as the sender sees it: an SMTP reply line.
type Rejection struct{ SMTP string }

func (r *Rejection) Error() string { return r.SMTP }

func reject(s string) error { return &Rejection{SMTP: s} }

const (
	rejectNoAddress = "550 5.1.1 no such address"
	rejectTooLarge  = "552 5.3.4 message too large"
)

// --- authentication verdict ----------------------------------------------------------------

func domainOf(address string) string {
	return strings.ToLower(address[strings.LastIndex(address, "@")+1:])
}

func aligned(authDomain, fromDomain string) bool {
	a := strings.TrimSuffix(strings.ToLower(authDomain), ".")
	return a == fromDomain || strings.HasSuffix(fromDomain, "."+a) || strings.HasSuffix(a, "."+fromDomain)
}

var (
	methodRe     = regexp.MustCompile(`(?i)^(dmarc|dkim|spf)\s*=\s*(\w+)`)
	headerFromRe = regexp.MustCompile(`(?i)header\.from\s*=\s*([^\s;]+)`)
	headerDRe    = regexp.MustCompile(`(?i)header\.d\s*=\s*([^\s;]+)`)
	mailFromRe   = regexp.MustCompile(`(?i)smtp\.mailfrom\s*=\s*([^\s;]+)`)
)

// SenderVerified reports whether the From domain authenticated a message, from the
// Authentication-Results header the receiving server added: DMARC passed, or DKIM or SPF passed
// for a domain aligned with From. Only the TOPMOST header counts, and only when it names our own
// receiving server (authservID) — every header below it came with the message.
func SenderVerified(headerValues []string, fromAddress, authservID string) bool {
	if len(headerValues) == 0 || !strings.Contains(fromAddress, "@") {
		return false
	}
	fromDomain := domainOf(fromAddress)
	parts := strings.Split(headerValues[0], ";")
	serv := strings.Fields(strings.TrimSpace(parts[0]))
	if len(serv) == 0 || !strings.EqualFold(serv[0], authservID) {
		return false
	}
	for _, r := range parts[1:] {
		r = strings.TrimSpace(r)
		m := methodRe.FindStringSubmatch(r)
		if m == nil || !strings.EqualFold(m[2], "pass") {
			continue
		}
		switch strings.ToLower(m[1]) {
		case "dmarc":
			hf := headerFromRe.FindStringSubmatch(r)
			if hf == nil || aligned(hf[1], fromDomain) {
				return true
			}
		case "dkim":
			if d := headerDRe.FindStringSubmatch(r); d != nil && aligned(d[1], fromDomain) {
				return true
			}
		default:
			if mf := mailFromRe.FindStringSubmatch(r); mf != nil {
				v := mf[1]
				if strings.Contains(v, "@") {
					v = domainOf(v)
				}
				if aligned(v, fromDomain) {
					return true
				}
			}
		}
	}
	return false
}

// authStamp is the Authentication-Results value the emulator's receiving side prepends to every
// message it takes in. Hosted the receiving mail server writes this from real SPF/DKIM/DMARC
// checks; locally there is nothing to check, so the caller SAYS whether the sender counts as
// authenticated (the console's "sender authenticated" box, inject's `authenticated=1`, the SMTP
// listener's --smtp-auth). Stamping every message — a fail as much as a pass — keeps the hosted
// rule intact: only the topmost header is believed, so a forged one inside the message is not.
func authStamp(authservID, fromAddress string, authenticated bool) string {
	d := "unknown"
	if strings.Contains(fromAddress, "@") {
		d = domainOf(fromAddress)
	}
	if authenticated {
		return fmt.Sprintf("%s; dmarc=pass (emulator: sender marked authenticated) header.from=%s", authservID, d)
	}
	return fmt.Sprintf("%s; dmarc=none (emulator: sender not marked authenticated) header.from=%s", authservID, d)
}

// --- admission -----------------------------------------------------------------------------

// instanceBySlug resolves an address's slug to an instance: locally, the instance named it.
func (s *Service) instanceBySlug(slug string) *control.Instance {
	if !ValidSlug(slug) {
		return nil
	}
	return s.reg.Get("mailbox", slug)
}

// AdmitAddress is everything that can refuse a message for a full address before its body is
// read. rawSize may be 0 when it is not yet known (SMTP without SIZE); the caller then checks the
// size after reading.
func (s *Service) AdmitAddress(to string, rawSize int64) (*control.Instance, Recipient, error) {
	r, ok := ParseRecipient(to, s.domain)
	if !ok {
		return nil, Recipient{}, reject(rejectNoAddress)
	}
	inst := s.instanceBySlug(r.Slug)
	if inst == nil {
		return nil, Recipient{}, reject(rejectNoAddress)
	}
	if rawSize > MaxMessageBytes {
		return nil, Recipient{}, reject(rejectTooLarge)
	}
	if err := s.admitLocal(inst, r.Local); err != nil {
		return nil, Recipient{}, err
	}
	return inst, r, nil
}

// admitLocal is the per-address half of admission: disabled, unregistered, over the day's cap.
func (s *Service) admitLocal(inst *control.Instance, local string) error {
	st := ParseConfig(inst.Config()).Settings
	res, err := s.store.Admit(inst.ID, local, s.now(), st.AcceptMode, st.InboundDailyCap)
	if err != nil {
		return err
	}
	if !res.OK {
		if res.Code == 452 {
			log.Printf("[mailbox] %s is over its inbound cap of %d for today", inst.Name, st.InboundDailyCap)
		}
		return reject(res.Reason)
	}
	return nil
}

// Deliver is the whole inbound path for a full address: admit, then ingest.
func (s *Service) Deliver(to, envelopeFrom string, raw []byte, authenticated bool) (*Record, error) {
	inst, r, err := s.AdmitAddress(to, int64(len(raw)))
	if err != nil {
		return nil, err
	}
	return s.Ingest(inst, Local{Local: r.Local, Tag: r.Tag}, envelopeFrom, raw, authenticated)
}

// DeliverLocal delivers to a bare `name[+tag]` (or a full address on this instance) of a known
// instance — what inject and the console do, with or without a valid slug.
func (s *Service) DeliverLocal(inst *control.Instance, to, envelopeFrom string, raw []byte, authenticated bool) (*Record, error) {
	if len(raw) > MaxMessageBytes {
		return nil, reject(rejectTooLarge)
	}
	var target Local
	if strings.Contains(to, "@") {
		r, ok := ParseRecipient(to, s.domain)
		if !ok || r.Slug != SlugOf(inst.Name) {
			return nil, reject(rejectNoAddress)
		}
		target = Local{Local: r.Local, Tag: r.Tag}
	} else {
		p, ok := ParseLocal(to)
		if !ok {
			return nil, reject(rejectNoAddress)
		}
		target = p
	}
	if err := s.admitLocal(inst, target.Local); err != nil {
		return nil, err
	}
	return s.Ingest(inst, target, envelopeFrom, raw, authenticated)
}

var safeTypeRe = regexp.MustCompile(`(?i)^[a-z0-9][a-z0-9!#$&^_.+-]*/[a-z0-9][a-z0-9!#$&^_.+-]*$`)

func storedType(t string) string {
	v := strings.ToLower(strings.TrimSpace(strings.Split(t, ";")[0]))
	if safeTypeRe.MatchString(v) && len(v) <= 127 {
		return v
	}
	return "application/octet-stream"
}

var ctlRe = regexp.MustCompile(`[\x00-\x1f\x7f"\\/]`)

func cleanFilename(f string) *string {
	if f == "" {
		return nil
	}
	v := strings.TrimSpace(ctlRe.ReplaceAllString(f, "_"))
	if r := []rune(v); len(r) > 200 {
		v = string(r[:200])
	}
	if v == "" {
		return nil
	}
	return &v
}

func optStr(s string, max int) *string {
	if s == "" {
		return nil
	}
	if len(s) > max {
		s = s[:max]
	}
	return &s
}

// Ingest parses, stores and dispatches one message that has already been admitted.
func (s *Service) Ingest(inst *control.Instance, to Local, envelopeFrom string, raw []byte, authenticated bool) (*Record, error) {
	now := s.now()
	cfg := ParseConfig(inst.Config())
	e := ParseEmail(raw)
	id := NewMessageID(now)
	from := AddressValue{Address: envelopeFrom}
	if len(e.From) > 0 {
		from = e.From[0]
	}
	stamp := authStamp(s.domain, from.Address, authenticated)
	verified := SenderVerified(append([]string{stamp}, e.AuthResults...), from.Address, s.domain)
	stored := append([]byte("Authentication-Results: "+stamp+"\r\n"), raw...)

	attachments := make([]Attachment, 0, len(e.Attachments))
	var storedBytes int64
	for idx, a := range e.Attachments {
		att := Attachment{
			Idx:         idx,
			Filename:    cleanFilename(a.Filename),
			ContentType: storedType(a.MimeType),
			Size:        int64(len(a.Content)),
			ContentID:   optStr(a.ContentID, 200),
			Disposition: optStr(a.Disposition, 200),
		}
		switch {
		case idx >= MaxAttachments:
			r := "too many attachments"
			att.Rejected = &r
		case len(a.Content) > MaxAttachmentBytes:
			r := "too large"
			att.Rejected = &r
		default:
			att.Key = AttachmentKey(id, idx)
			if err := s.store.PutBytes(inst.ID, att.Key, a.Content); err != nil {
				return nil, err
			}
			storedBytes += att.Size
		}
		attachments = append(attachments, att)
	}
	rawKey := ""
	if cfg.Settings.KeepRaw {
		rawKey = RawKey(id)
		if err := s.store.PutBytes(inst.ID, rawKey, stored); err != nil {
			return nil, err
		}
		storedBytes += int64(len(stored))
	}
	subject := strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(e.Subject)
	msg := NewMessage{
		ID: id, Direction: "in", Local: to.Local, PlusTag: to.Tag, From: from,
		To: e.To, Cc: e.Cc, ReplyTo: e.ReplyTo, Subject: subject, Text: e.Text, HTML: e.HTML,
		MessageID: e.MessageID, InReplyTo: e.InReplyTo, References: e.References,
		AuthResults: *optStr(stamp, 2000), Verified: verified, Status: "received", RawKey: rawKey,
		Size: int64(len(raw)), StoredBytes: storedBytes, Attachments: attachments,
		ReceivedAt: now, ExpiresAt: now + cfg.Settings.RetentionDays*86_400_000,
	}
	kept := 0
	for _, a := range attachments {
		if a.Rejected == nil {
			kept++
		}
	}
	var actions []string
	if route := MatchRoute(cfg.Routes, MatchInput{Local: msg.Local, From: from.Address, Subject: subject, AttachmentCount: kept}); route != nil {
		for _, a := range route.Do {
			actions = append(actions, ActionKey(a))
		}
	}
	if _, err := s.store.StoreMessage(inst.ID, msg, actions); err != nil {
		return nil, err
	}
	rec, err := s.store.Get(inst.ID, id)
	if err != nil || rec == nil {
		return nil, fmt.Errorf("reading back message %s: %v", id, err)
	}
	log.Printf("[mailbox] %s received %s for %q from %s (%d route action(s))", inst.Name, id, msg.Local, from.Address, len(actions))
	s.deliverAsync(inst, rec, actions)
	return rec, nil
}

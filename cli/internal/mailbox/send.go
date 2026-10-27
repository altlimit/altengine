package mailbox

import (
	"encoding/base64"
	"fmt"
	"log"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// Sending. NOTHING SENT HERE LEAVES THE EMULATOR: a send is checked against every rule the hosted
// service applies, recorded as a sent message, and — with loopback on — delivered into the local
// mailbox it is addressed to, if any.
//
// THE LIMITS, in the hosted order:
//  1. the instance's send mode — `off`, `reply` (only to someone who wrote to us in authenticated
//     mail), or `any`;
//  2. the From address is not disabled, the reply rule holds, and the instance's own daily cap
//     (optional) has room;
//  3. the org's daily cap — see OrgSendsPerDay for the emulator's choice.
// A slot taken at step 2 is given back if step 3 refuses.

const (
	// MaxRecipients is recipients per message, to and cc together.
	MaxRecipients = 50
	// MaxSendBodyBytes is the body cap per part on a send.
	MaxSendBodyBytes = 512 * 1024
	// MaxSendAttachments is attachments per sent message.
	MaxSendAttachments = 32
	// MaxSendMessageBytes is the whole sent message, encoded, attachments included.
	MaxSendMessageBytes = 5 * 1024 * 1024
	// MaxSendRequestBytes is the largest request carrying a send: the largest message base64'd
	// into JSON, with room for the rest.
	MaxSendRequestBytes = 8 * 1024 * 1024
)

// outboundPart is one validated, decoded attachment of a send.
type outboundPart struct {
	Filename    string
	ContentType string
	ContentID   string // "" = an ordinary attachment; set = shown inline as cid:<ContentID>
	Bytes       []byte
	base64Len   int
}

var contentIDRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,100}$`)

// decodeBase64 is the forgiving base64 decode a browser's atob does, with the hosted pre-check:
// whitespace anywhere is ignored, padding is optional, and anything outside the alphabet is
// refused.
func decodeBase64(s, field string) ([]byte, error) {
	bad := common.BadRequest(field + " must be base64")
	clean := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	if len(clean)%4 == 1 {
		return nil, bad
	}
	for i := 0; i < len(clean); i++ {
		c := clean[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=') {
			return nil, bad
		}
	}
	if len(clean)%4 == 0 {
		switch {
		case strings.HasSuffix(clean, "=="):
			clean = clean[:len(clean)-2]
		case strings.HasSuffix(clean, "="):
			clean = clean[:len(clean)-1]
		}
	}
	if len(clean)%4 == 1 || strings.Contains(clean, "=") {
		return nil, bad
	}
	out, err := base64.RawStdEncoding.DecodeString(clean)
	if err != nil {
		return nil, bad
	}
	return out, nil
}

// parseAttachments validates and decodes the attachments of a send, with the hosted rules and
// messages. Over HTTP and MCP `content` is base64 text; a function's bytes arrive here already
// base64'd by its binding.
func parseAttachments(v any) ([]outboundPart, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, common.BadRequest("attachments must be an array")
	}
	if len(list) > MaxSendAttachments {
		return nil, common.BadRequest(fmt.Sprintf("at most %d attachments per message", MaxSendAttachments))
	}
	out := make([]outboundPart, 0, len(list))
	for i, raw := range list {
		field := fmt.Sprintf("attachments[%d]", i)
		a, ok := raw.(map[string]any)
		if !ok {
			return nil, common.BadRequest(field + " must be an object")
		}
		content, ok := a["content"].(string)
		if !ok {
			return nil, common.BadRequest(field + ".content is required: base64 text, or bytes from a function")
		}
		b, err := decodeBase64(content, field+".content")
		if err != nil {
			return nil, err
		}
		if len(b) == 0 {
			return nil, common.BadRequest(field + ".content is empty")
		}
		filename, ok := a["filename"].(string)
		if !ok && a["filename"] != nil {
			return nil, common.BadRequest(field + ".filename must be a string")
		}
		contentType, ok := a["contentType"].(string)
		if !ok && a["contentType"] != nil {
			return nil, common.BadRequest(field + ".contentType must be a string")
		}
		cid := ""
		if a["contentId"] != nil {
			s, ok := a["contentId"].(string)
			if !ok || !contentIDRe.MatchString(s) {
				return nil, common.BadRequest(field + ".contentId must be 1–100 letters, digits, '.', '_', '@' or '-'")
			}
			cid = s
		}
		name := fmt.Sprintf("attachment-%d", i+1)
		if f := cleanFilename(filename); f != nil {
			name = *f
		}
		out = append(out, outboundPart{
			Filename: name, ContentType: storedType(contentType), ContentID: cid, Bytes: b,
			base64Len: base64.StdEncoding.EncodedLen(len(b)),
		})
	}
	return out, nil
}

// encodedSize is how large the sent message will be once encoded — conservatively, and by the
// hosted formula, so a message refused there is refused here: each body grows by a tenth plus its
// headers, each part is base64 with a CRLF every 76 characters plus its headers, and the message's
// own headers are allowed 16 KB.
func encodedSize(text, html string, parts []outboundPart) int64 {
	body := func(s string) int64 {
		if s == "" {
			return 0
		}
		return int64(math.Ceil(float64(len(s))*1.1)) + 512
	}
	var n int64
	for _, p := range parts {
		n += (int64(p.base64Len)*78+75)/76 + 512
	}
	return body(text) + body(html) + n + 16*1024
}

// StatusNotDelivered is a sent message's status when no local mailbox took it.
const StatusNotDelivered = "sent (emulator: not delivered)"

var reRe = regexp.MustCompile(`(?i)^re:`)

func addressList(v any, field string) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		list = []any{v}
	}
	out := make([]string, 0, len(list))
	for i, a := range list {
		s, err := ValidateOutboundAddress(a, fmt.Sprintf("%s[%d]", field, i))
		if err != nil {
			return nil, err
		}
		out = append(out, strings.ToLower(s))
	}
	return out, nil
}

func sendBody(v any, field string) (string, error) {
	if v == nil || v == "" {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", common.BadRequest(field + " must be a string")
	}
	if len(s) > MaxSendBodyBytes {
		return "", common.BadRequest(fmt.Sprintf("%s is larger than %d bytes", field, MaxSendBodyBytes))
	}
	return s, nil
}

func displayName(v any) (string, error) {
	if v == nil || v == "" {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", common.BadRequest("fromName must be a string")
	}
	s = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "<", " ", ">", " ", `"`, " ", `\`, " ").Replace(s))
	if r := []rune(s); len(r) > 100 {
		s = strings.TrimSpace(string(r[:100]))
	}
	return s, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Send sends one message from inst. Every surface — /v1, MCP, the function binding and the
// console — calls this, so the limits have one implementation. `input` is the JSON body:
// from, fromName, to, cc, subject, text, html, attachments, replyToMessageId.
func (s *Service) Send(inst *control.Instance, input map[string]any) (*Summary, error) {
	now := s.now()
	cfg := ParseConfig(inst.Config())
	st := cfg.Settings
	if st.SendMode == "off" {
		return nil, common.NewError(403, "sending is off for this mailbox (settings.sendMode)", "SEND_DISABLED")
	}
	slug := SlugOf(inst.Name)
	if slug == "" {
		return nil, common.BadRequest(fmt.Sprintf("mailbox instance '%s' has no address locally: its name is used as the address slug, which must be 3–32 lowercase letters, digits and inner hyphens", inst.Name))
	}

	var original *Record
	if v, ok := input["replyToMessageId"]; ok && v != nil {
		id, ok := v.(string)
		if !ok {
			return nil, common.BadRequest("replyToMessageId must be a string")
		}
		m, err := s.store.Get(inst.ID, id)
		if err != nil {
			return nil, err
		}
		if m == nil {
			return nil, common.NotFound("the message being replied to was not found")
		}
		if m.Direction != "in" {
			return nil, common.BadRequest("replyToMessageId must name a received message")
		}
		original = m
	}

	fromRaw := input["from"]
	if fromRaw == nil {
		if original != nil {
			fromRaw = original.Local
		} else {
			fromRaw = DefaultLocal
		}
	}
	local, err := ValidateLocal(fromRaw, slug)
	if err != nil {
		return nil, err
	}
	to, err := addressList(input["to"], "to")
	if err != nil {
		return nil, err
	}
	if len(to) == 0 && original != nil {
		who := original.From
		if len(original.ReplyTo) > 0 {
			who = original.ReplyTo[0]
		}
		to = []string{strings.ToLower(who.Address)}
	}
	cc, err := addressList(input["cc"], "cc")
	if err != nil {
		return nil, err
	}
	if len(to) == 0 {
		return nil, common.BadRequest("to must name at least one recipient")
	}
	if len(to)+len(cc) > MaxRecipients {
		return nil, common.BadRequest(fmt.Sprintf("at most %d recipients per message", MaxRecipients))
	}
	subject := ""
	if v, ok := input["subject"].(string); ok {
		subject = strings.TrimSpace(strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(v))
	}
	if subject == "" && original != nil {
		subject = original.Subject
		if !reRe.MatchString(subject) {
			subject = "Re: " + subject
		}
	}
	if subject == "" {
		return nil, common.BadRequest("subject is required")
	}
	if len([]rune(subject)) > 998 {
		return nil, common.BadRequest("subject is longer than 998 characters")
	}
	text, err := sendBody(input["text"], "text")
	if err != nil {
		return nil, err
	}
	html, err := sendBody(input["html"], "html")
	if err != nil {
		return nil, err
	}
	if text == "" && html == "" {
		return nil, common.BadRequest("text or html is required")
	}
	name, err := displayName(input["fromName"])
	if err != nil {
		return nil, err
	}
	parts, err := parseAttachments(input["attachments"])
	if err != nil {
		return nil, err
	}
	// Refused here, before any send slot is taken, rather than by the mail service after.
	if encodedSize(text, html, parts) > MaxSendMessageBytes {
		return nil, common.NewError(413, fmt.Sprintf("the message is larger than %d bytes once encoded (attachments grow by a third); send less, or link to the file instead", MaxSendMessageBytes), "INVALID_ARGUMENT")
	}
	inReplyTo, references := "", ""
	if original != nil && original.MessageID != nil {
		inReplyTo = *original.MessageID
		refs := strings.TrimSpace(deref(original.References) + " " + *original.MessageID)
		if len(refs) > 4000 {
			refs = refs[:4000]
		}
		references = refs
	}

	if err := s.store.PrepareSend(inst.ID, local, append(append([]string{}, to...), cc...), st.SendMode == "reply", st.DailySendCap, now); err != nil {
		return nil, err
	}
	if !s.takeOrgSend(now) {
		s.store.ReturnSend(inst.ID, now)
		return nil, common.NewError(429, fmt.Sprintf("this organization's daily send limit of %d is reached; it resets at 00:00 UTC", OrgSendsPerDay), "SEND_LIMIT")
	}

	fromAddr := AddressFor(slug, local, s.domain)
	id := NewMessageID(now)
	messageID := "<" + id + "@" + s.domain + ">"
	status := StatusNotDelivered
	if s.Loopback() {
		out := OutMessage{
			From: AddressValue{Address: fromAddr, Name: name}, To: asValues(to), Cc: asValues(cc),
			Subject: subject, Text: text, HTML: html, MessageID: messageID, InReplyTo: inReplyTo,
			References: references, Date: time.UnixMilli(now),
		}
		for _, p := range parts {
			out.Attachments = append(out.Attachments, OutAttachment{Filename: p.Filename, ContentType: p.ContentType, Content: p.Bytes, ContentID: p.ContentID})
		}
		status = s.loopbackDeliver(out, append(append([]string{}, to...), cc...))
	}
	log.Printf("[mailbox] %s sent %s from %s to %s (%d attachment(s)): %s", inst.Name, id, fromAddr, strings.Join(append(append([]string{}, to...), cc...), ","), len(parts), status)

	// A copy of what was sent, so the Sent message lists its attachments like a received one. The
	// mail has already gone: a part that cannot be stored is listed as such, not a failed send.
	attachments := make([]Attachment, 0, len(parts))
	var storedBytes, size int64
	var keys []string
	for idx, p := range parts {
		fn, ct := p.Filename, p.ContentType
		disp := "attachment"
		var cid *string
		if p.ContentID != "" {
			disp = "inline"
			v := p.ContentID
			cid = &v
		}
		a := Attachment{Idx: idx, Filename: &fn, ContentType: ct, Size: int64(len(p.Bytes)), ContentID: cid, Disposition: &disp}
		key := AttachmentKey(id, idx)
		if err := s.store.PutBytes(inst.ID, key, p.Bytes); err != nil {
			log.Printf("[mailbox] storing sent attachment %s failed: %v", key, err)
			why := "not stored"
			a.Rejected = &why
		} else {
			a.Key = key
			keys = append(keys, key)
			storedBytes += a.Size
		}
		size += a.Size
		attachments = append(attachments, a)
	}

	var tp, hp *string
	if text != "" {
		tp = &text
	}
	if html != "" {
		hp = &html
	}
	sum, err := s.store.StoreMessage(inst.ID, NewMessage{
		ID: id, Direction: "out", Local: local, From: AddressValue{Address: fromAddr, Name: name},
		To: asValues(to), Cc: asValues(cc), ReplyTo: []AddressValue{}, Subject: subject, Text: tp, HTML: hp,
		MessageID: messageID, InReplyTo: inReplyTo, References: references, Verified: false, Status: status,
		Size: int64(len(text)+len(html)) + size, StoredBytes: storedBytes, Attachments: attachments,
		ReceivedAt: now, ExpiresAt: now + st.RetentionDays*86_400_000,
	}, nil)
	if err != nil {
		// The bytes would otherwise sit with no row naming them until the instance is deleted.
		s.store.deleteBytes(inst.ID, keys)
		return nil, err
	}
	return &sum, nil
}

func asValues(list []string) []AddressValue {
	out := make([]AddressValue, 0, len(list))
	for _, a := range list {
		out = append(out, AddressValue{Address: a, Name: ""})
	}
	return out
}

// loopbackDeliver delivers a sent message to every recipient that is an address on a local
// mailbox, through the ordinary inbound path, and says what happened as the sent message's
// status. Mail from a mailbox address is marked authenticated: hosted, the platform signs what
// it sends, so the receiving side would see it pass.
func (s *Service) loopbackDeliver(m OutMessage, recipients []string) string {
	raw := BuildMessage(m)
	var delivered []string
	var refused []string
	for _, rcpt := range recipients {
		if _, ok := ParseRecipient(rcpt, s.domain); !ok {
			continue
		}
		if _, err := s.Deliver(rcpt, m.From.Address, raw, true); err != nil {
			refused = append(refused, rcpt+": "+err.Error())
			continue
		}
		delivered = append(delivered, rcpt)
	}
	switch {
	case len(delivered) == 0 && len(refused) == 0:
		return StatusNotDelivered
	case len(refused) == 0:
		return "sent (emulator: delivered locally to " + strings.Join(delivered, ", ") + ")"
	}
	parts := []string{}
	if len(delivered) > 0 {
		parts = append(parts, "delivered locally to "+strings.Join(delivered, ", "))
	}
	parts = append(parts, "refused by "+strings.Join(refused, "; "))
	return "sent (emulator: " + strings.Join(parts, "; ") + ")"
}

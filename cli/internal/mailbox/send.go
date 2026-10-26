package mailbox

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

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
)

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
// from, fromName, to, cc, subject, text, html, replyToMessageId.
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
		status = s.loopbackDeliver(OutMessage{
			From: AddressValue{Address: fromAddr, Name: name}, To: asValues(to), Cc: asValues(cc),
			Subject: subject, Text: text, HTML: html, MessageID: messageID, InReplyTo: inReplyTo,
			References: references, Date: time.UnixMilli(now),
		}, append(append([]string{}, to...), cc...))
	}
	log.Printf("[mailbox] %s sent %s from %s to %s: %s", inst.Name, id, fromAddr, strings.Join(append(append([]string{}, to...), cc...), ","), status)

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
		Size: int64(len(text) + len(html)), ReceivedAt: now, ExpiresAt: now + st.RetentionDays*86_400_000,
	}, nil)
	if err != nil {
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

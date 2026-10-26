package mailbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/altlimit/altengine/cli/internal/channel"
	"github.com/altlimit/altengine/cli/internal/common"
	"github.com/altlimit/altengine/cli/internal/control"
)

// Incoming-mail triggers: calling a function, publishing to a channel, or POSTing a webhook when
// mail arrives.
//
// The function and the channel are IN-PROCESS, as hosted: the function is dispatched into the
// emulator's own router with the "mailbox" trigger (exactly as a container's completion callback
// is), and the channel publish goes straight into the hub. A target is resolved again on every
// attempt, because it can be deleted after the route was saved; one that no longer exists is a
// permanent error rather than a retry.
//
// RETRIES. The first attempt runs as soon as the message is stored. A failure is written back as
// `pending` with a retry time, on the hosted schedule (1m, 5m, 30m, 2h), and the background loop
// re-drives it. Five attempts, then `error:<why>`.

const (
	// FunctionTimeout is how long one function call is waited for.
	FunctionTimeout = 30 * time.Second
	// WebhookTimeout is how long a webhook receiver is waited for.
	WebhookTimeout = 10 * time.Second
	// FunctionBodyMax is the body handed to a function (and a webhook), per part.
	FunctionBodyMax = 256 * 1024
	// attachmentURLTTL is how long an attachment URL in a payload stays valid.
	attachmentURLTTL = time.Hour
	// deliveryGrace is how long a never-tried delivery is left to its first attempt before the
	// loop would take it over — what keeps the loop from racing the inline attempt.
	deliveryGrace = 60 * time.Second

	SignatureHeader = "x-altengine-signature"
	TimestampHeader = "x-altengine-timestamp"
)

type outcome struct {
	ok        bool
	status    string
	why       string
	permanent bool
}

// summaryPayload is every envelope and header field a subscriber might filter on.
func summaryPayload(instanceName string, m *Record) map[string]any {
	return map[string]any{
		"type":       "mailbox.received",
		"instance":   instanceName,
		"id":         m.ID,
		"local":      m.Local,
		"plusTag":    m.PlusTag,
		"to":         m.To,
		"cc":         m.Cc,
		"from":       m.From,
		"replyTo":    m.ReplyTo,
		"subject":    m.Subject,
		"snippet":    m.Snippet,
		"messageId":  m.MessageID,
		"inReplyTo":  m.InReplyTo,
		"verified":   m.Verified,
		"receivedAt": m.ReceivedAt,
		"size":       m.Size,
	}
}

// ClipBytes cuts s to at most maxBytes of UTF-8 without leaving half a character.
func ClipBytes(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	if maxBytes <= 0 {
		return "", true
	}
	cut := s[:maxBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut, true
}

// ChannelPayload is the channel message: envelope fields, no body unless asked, never an
// attachment URL — subscribers can be end users' browsers. Sized to fit the channel's message cap
// once framed as {channel, data, ts}.
func ChannelPayload(instanceName, ch string, m *Record, includeText bool) map[string]any {
	base := summaryPayload(instanceName, m)
	atts := []map[string]any{}
	for _, a := range m.Attachments {
		if a.Rejected != nil {
			continue
		}
		atts = append(atts, map[string]any{"idx": a.Idx, "filename": a.Filename, "contentType": a.ContentType, "size": a.Size})
	}
	base["attachments"] = atts
	if !includeText {
		return base
	}
	frame := func(d any) int {
		b, _ := json.Marshal(map[string]any{"channel": ch, "data": d, "ts": time.Now().UnixMilli()})
		return len(b)
	}
	probe := map[string]any{}
	for k, v := range base {
		probe[k] = v
	}
	probe["text"], probe["textTruncated"] = "", true
	room := (channel.MaxMessageBytes - frame(probe) - 64) / 2
	if room < 0 {
		room = 0
	}
	text := ""
	if m.Text != nil {
		text = *m.Text
	}
	t, cut := ClipBytes(text, room)
	base["text"], base["textTruncated"] = t, cut || m.TextTruncated
	return base
}

// FunctionPayload is the function (and webhook) request body: the summary, the bodies (bounded),
// and attachments with short-lived download URLs.
func (s *Service) FunctionPayload(inst *control.Instance, m *Record) map[string]any {
	out := summaryPayload(inst.Name, m)
	text, textCut := Clip(m.Text, FunctionBodyMax)
	html, htmlCut := Clip(m.HTML, FunctionBodyMax)
	atts := []map[string]any{}
	for _, a := range m.Attachments {
		if a.Rejected != nil || a.Key == "" {
			continue
		}
		atts = append(atts, map[string]any{
			"idx": a.Idx, "filename": a.Filename, "contentType": a.ContentType, "size": a.Size, "contentId": a.ContentID,
			"url": s.signedURL(s.base, inst.ID, m.ID, strconv.Itoa(a.Idx), attachmentURLTTL),
		})
	}
	out["text"], out["textTruncated"] = text, textCut || m.TextTruncated
	out["html"], out["htmlTruncated"] = html, htmlCut || m.HTMLTruncated
	out["attachments"] = atts
	out["attachmentUrlsExpireAt"] = time.Now().Add(attachmentURLTTL).UnixMilli()
	return out
}

func (s *Service) callFunction(inst *control.Instance, targetName string, m *Record) outcome {
	fnInst, name := SplitTarget(targetName)
	if s.reg.Get("functions", fnInst) == nil {
		return outcome{why: fmt.Sprintf("no functions instance '%s'", fnInst), permanent: true}
	}
	if s.mux == nil {
		return outcome{why: "functions are not served here", permanent: true}
	}
	body, _ := json.Marshal(s.FunctionPayload(inst, m))
	// Marked as a trigger: a payload addressed to a function that no longer exists must 404,
	// not reach the instance's default function.
	req := common.WithTrigger(httptest.NewRequest(http.MethodPost, "/fn/"+url.PathEscape(fnInst)+"/"+url.PathEscape(name), bytes.NewReader(body)), "mailbox")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer emulator-internal")
	req.Header.Set("user-agent", "altengine-mailbox")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.mux.ServeHTTP(rec, req)
	}()
	select {
	case <-done:
	case <-time.After(FunctionTimeout):
		return outcome{why: fmt.Sprintf("no answer within %ds", int(FunctionTimeout/time.Second))}
	}
	if rec.Code >= 200 && rec.Code < 300 {
		return outcome{ok: true, status: fmt.Sprintf("ok:%d", rec.Code)}
	}
	return outcome{why: fmt.Sprintf("function answered %d", rec.Code)}
}

func (s *Service) publish(targetName string, includeText bool, m *Record, mailboxName string) outcome {
	chInst, name := SplitTarget(targetName)
	in := s.reg.Get("channel", chInst)
	if in == nil {
		return outcome{why: fmt.Sprintf("no channel instance '%s'", chInst), permanent: true}
	}
	if s.hub == nil {
		return outcome{why: "channels are not served here", permanent: true}
	}
	data, _ := json.Marshal(ChannelPayload(mailboxName, name, m, includeText))
	n, err := s.hub.Publish(in.ID, name, data)
	if err != nil {
		return outcome{why: err.Error()}
	}
	return outcome{ok: true, status: fmt.Sprintf("ok:%d subscribers", n)}
}

// SignWebhook is the delivery signature: `v1=` + hex HMAC-SHA256, keyed with the whole secret,
// over "<timestamp>.<raw body>" — the scheme every altengine webhook uses.
func SignWebhook(secret string, timestamp int64, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(h, "%d.", timestamp)
	h.Write(body)
	return "v1=" + hex.EncodeToString(h.Sum(nil))
}

func (s *Service) postWebhook(inst *control.Instance, target string, m *Record) outcome {
	key, err := s.store.WebhookSecret(inst.ID)
	if err != nil || key == "" {
		// Nothing is sent unsigned.
		return outcome{why: "signing key unavailable"}
	}
	body, _ := json.Marshal(s.FunctionPayload(inst, m))
	ts := time.Now().UnixMilli()
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return outcome{why: err.Error()}
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", "altengine-mailbox")
	req.Header.Set(SignatureHeader, SignWebhook(key, ts, body))
	req.Header.Set(TimestampHeader, strconv.FormatInt(ts, 10))
	res, err := s.client.Do(req)
	if err != nil {
		if ue, ok := err.(*url.Error); ok && ue.Timeout() {
			return outcome{why: fmt.Sprintf("no answer within %ds", int(WebhookTimeout/time.Second))}
		}
		return outcome{why: err.Error()}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return outcome{ok: true, status: fmt.Sprintf("ok:%d", res.StatusCode)}
	}
	return outcome{why: fmt.Sprintf("receiver answered %d", res.StatusCode)}
}

func (s *Service) runAction(inst *control.Instance, a Action, m *Record) outcome {
	switch {
	case a.Function != "":
		return s.callFunction(inst, a.Function, m)
	case a.Webhook != "":
		return s.postWebhook(inst, a.Webhook, m)
	}
	return s.publish(a.Channel, a.IncludeText, m, inst.Name)
}

// attempt tries each delivery once and says what to record. attempts is how many were made before.
func (s *Service) attempt(inst *control.Instance, m *Record, pending []DueDelivery, now int64) []DeliveryResult {
	out := make([]DeliveryResult, 0, len(pending))
	for _, p := range pending {
		tries := p.Attempts + 1
		a, ok := ParseActionKey(p.Action)
		if !ok {
			out = append(out, DeliveryResult{MessageID: m.ID, Action: p.Action, Status: "error:unknown action", Attempts: tries})
			continue
		}
		o := s.runAction(inst, a, m)
		if o.ok {
			out = append(out, DeliveryResult{MessageID: m.ID, Action: p.Action, Status: o.status, Attempts: tries})
			continue
		}
		var next int64
		if !o.permanent {
			next = RetryAt(tries, now)
		}
		status := "pending"
		if next == 0 {
			status = "error:" + o.why
			if len(status) > 500 {
				status = status[:500]
			}
		}
		out = append(out, DeliveryResult{MessageID: m.ID, Action: p.Action, Status: status, Attempts: tries, NextAt: next})
	}
	return out
}

func (s *Service) claim(instanceID, messageID string, actions []string) []string {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	var mine []string
	for _, a := range actions {
		k := instanceID + "\x00" + messageID + "\x00" + a
		if s.inflight[k] {
			continue
		}
		s.inflight[k] = true
		mine = append(mine, a)
	}
	return mine
}

func (s *Service) release(instanceID, messageID string, actions []string) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	for _, a := range actions {
		delete(s.inflight, instanceID+"\x00"+messageID+"\x00"+a)
	}
}

// deliverAsync fires a just-stored message's deliveries in the background.
func (s *Service) deliverAsync(inst *control.Instance, m *Record, actions []string) {
	if len(actions) == 0 {
		return
	}
	mine := s.claim(inst.ID, m.ID, actions)
	if len(mine) == 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.release(inst.ID, m.ID, mine)
		s.deliver(inst, m, mine, 0)
	}()
}

// DeliverNow runs a message's deliveries in the caller's goroutine — replay, and tests.
func (s *Service) deliverNow(inst *control.Instance, m *Record, actions []string) {
	mine := s.claim(inst.ID, m.ID, actions)
	defer s.release(inst.ID, m.ID, mine)
	s.deliver(inst, m, mine, 0)
}

func (s *Service) deliver(inst *control.Instance, m *Record, actions []string, attempts int) {
	pending := make([]DueDelivery, 0, len(actions))
	for _, a := range actions {
		pending = append(pending, DueDelivery{MessageID: m.ID, Action: a, Attempts: attempts})
	}
	results := s.attempt(inst, m, pending, s.now())
	if err := s.store.RecordDeliveries(inst.ID, results); err != nil {
		log.Printf("[mailbox] recording deliveries for %s/%s: %v", inst.Name, m.ID, err)
	}
	for _, r := range results {
		log.Printf("[mailbox] %s %s → %s: %s", inst.Name, m.ID, r.Action, r.Status)
	}
}

// Replay re-runs a stored message through the instance's CURRENT routes: the deliveries are
// replaced with what the routes pick now and attempted at once. Returns the message as it stands
// afterwards.
func (s *Service) Replay(inst *control.Instance, id string) (*Record, error) {
	m, err := s.store.Get(inst.ID, id)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, common.NotFound("message not found")
	}
	if m.Direction != "in" {
		return nil, common.BadRequest("only a received message can be replayed")
	}
	cfg := ParseConfig(inst.Config())
	var actions []string
	if route := MatchRoute(cfg.Routes, MatchInput{Local: m.Local, From: m.From.Address, Subject: m.Subject, AttachmentCount: m.AttachmentCount}); route != nil {
		for _, a := range route.Do {
			actions = append(actions, ActionKey(a))
		}
	}
	if err := s.store.ResetDeliveries(inst.ID, id, actions, s.now()); err != nil {
		return nil, err
	}
	s.deliverNow(inst, m, actions)
	return s.store.Get(inst.ID, id)
}

// Tick is one pass of the background loop: expire what has passed its time and re-drive
// deliveries that are due.
func (s *Service) Tick() {
	now := s.now()
	for _, inst := range s.reg.List("mailbox") {
		if n, err := s.store.Expire(inst.ID, now); err == nil && n > 0 {
			log.Printf("[mailbox] %s: %d message(s) expired", inst.Name, n)
		}
		due, err := s.store.DueDeliveries(inst.ID, now, 20)
		if err != nil {
			continue
		}
		byMsg := map[string][]DueDelivery{}
		var order []string
		for _, d := range due {
			// A never-tried delivery belongs to its inline attempt until the grace has passed.
			if d.Attempts == 0 && !s.pastGrace(inst.ID, d.MessageID, now) {
				continue
			}
			if _, ok := byMsg[d.MessageID]; !ok {
				order = append(order, d.MessageID)
			}
			byMsg[d.MessageID] = append(byMsg[d.MessageID], d)
		}
		for _, id := range order {
			list := byMsg[id]
			keys := make([]string, 0, len(list))
			for _, d := range list {
				keys = append(keys, d.Action)
			}
			mine := s.claim(inst.ID, id, keys)
			if len(mine) == 0 {
				continue
			}
			m, err := s.store.Get(inst.ID, id)
			if err != nil || m == nil {
				var gone []DeliveryResult
				for _, d := range list {
					gone = append(gone, DeliveryResult{MessageID: id, Action: d.Action, Status: "error:message is gone", Attempts: d.Attempts})
				}
				_ = s.store.RecordDeliveries(inst.ID, gone)
				s.release(inst.ID, id, mine)
				continue
			}
			var pending []DueDelivery
			for _, d := range list {
				for _, k := range mine {
					if k == d.Action {
						pending = append(pending, d)
					}
				}
			}
			results := s.attempt(inst, m, pending, now)
			_ = s.store.RecordDeliveries(inst.ID, results)
			for _, r := range results {
				log.Printf("[mailbox] %s %s → %s (retry %d): %s", inst.Name, id, r.Action, r.Attempts, r.Status)
			}
			s.release(inst.ID, id, mine)
		}
	}
}

func (s *Service) pastGrace(instanceID, messageID string, now int64) bool {
	m, err := s.store.Get(instanceID, messageID)
	if err != nil || m == nil {
		return true
	}
	return now-m.ReceivedAt >= deliveryGrace.Milliseconds()
}

// Run drives Tick every few seconds until ctx is done.
func (s *Service) Run(ctx context.Context) {
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Tick()
			}
		}
	}()
}

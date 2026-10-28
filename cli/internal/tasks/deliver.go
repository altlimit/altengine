package tasks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/altlimit/altengine/cli/internal/channel"
	"github.com/altlimit/altengine/cli/internal/common"
)

// Delivering one task to its target, as hosted:
//
//   - a function is called IN PROCESS — dispatched into the emulator's own router with the
//     "task" trigger, so it sees `x-ae-trigger: task`, exactly as a scheduled run sees "cron";
//   - a channel publish goes straight into the hub;
//   - a webhook is POSTed, signed with the instance's key.
//
// A target names `<instance>/<name>` and is resolved again on every attempt — it can be deleted
// after the task was added. One that no longer resolves is a permanent failure, not a retry.

const (
	SignatureHeader = "x-altengine-signature"
	TimestampHeader = "x-altengine-timestamp"
)

// Envelope is what a target receives about the task it is handed.
type Envelope struct {
	ID           string          `json:"id"`
	Queue        string          `json:"queue"`
	Name         *string         `json:"name"`
	Attempt      int             `json:"attempt"`
	MaxAttempts  int             `json:"maxAttempts"`
	RunAt        int64           `json:"runAt"`
	CreatedAt    int64           `json:"createdAt"`
	Source       string          `json:"source"`
	Depth        int             `json:"depth"`
	Params       json.RawMessage `json:"params"`
	DeadLetterOf *string         `json:"deadLetterOf"`
	// DeadlineAt is when the queue stops waiting for this attempt and counts it as failed, in
	// epoch ms. The target is not stopped then; it can still be running when the retry starts.
	DeadlineAt int64 `json:"deadlineAt"`
}

// deadline is how long one attempt is waited for: until the envelope's deadline, at least a
// second. The message names the deadline itself.
func (s *Service) deadline(task Envelope) (wait time.Duration, timedOut string) {
	ms := task.DeadlineAt - s.now()
	if ms < 1000 {
		ms = 1000
	}
	wait = time.Duration(ms) * time.Millisecond
	timedOut = fmt.Sprintf("no answer within the %ds deadline", (ms+500)/1000)
	if s.waitCap > 0 && s.waitCap < wait {
		wait = s.waitCap
	}
	return wait, timedOut
}

// clipBytes cuts s to at most max bytes of UTF-8 without leaving half a character.
func clipBytes(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut, true
}

// DeliveryBody is the JSON a function or webhook receives: the payload spliced in as stored.
func DeliveryBody(task Envelope, payloadJSON string) []byte {
	t, _ := json.Marshal(task)
	var b bytes.Buffer
	b.WriteString(`{"task":`)
	b.Write(t)
	b.WriteString(`,"payload":`)
	b.WriteString(payloadJSON)
	b.WriteString(`}`)
	return b.Bytes()
}

// SignWebhook is the delivery signature: `v1=` + hex HMAC-SHA256, keyed with the whole secret,
// over "<timestamp>.<raw body>" — the scheme every altengine webhook uses.
func SignWebhook(secret string, timestamp int64, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(h, "%d.", timestamp)
	h.Write(body)
	return "v1=" + hex.EncodeToString(h.Sum(nil))
}

func setTaskHeaders(h http.Header, task Envelope) {
	h.Set("content-type", "application/json")
	h.Set("x-task-id", task.ID)
	h.Set("x-task-queue", task.Queue)
	h.Set("x-task-attempt", strconv.Itoa(task.Attempt))
	h.Set("user-agent", "altengine-tasks")
}

// A 4xx other than 408/429 will not improve by retrying — the target refused the payload.
func permanentStatus(status int) bool {
	return status >= 400 && status < 500 && status != 408 && status != 429
}

func resultOf(body []byte) *string {
	if len(body) == 0 {
		return nil
	}
	s, _ := clipBytes(string(body), MaxResultBytes)
	return &s
}

func (s *Service) deliver(instanceID string, target *Target, task Envelope, payloadJSON string) (out Outcome) {
	defer func() {
		if p := recover(); p != nil {
			out = Outcome{Status: "error", Error: fmt.Sprint(p)}
		}
	}()
	switch {
	case target == nil:
		return Outcome{Status: "error", Error: "no target: the task has none and neither does its queue", Permanent: true}
	case target.Function != "":
		return s.toFunction(target.Function, task, payloadJSON)
	case target.Channel != "":
		return s.toChannel(target.Channel, task, payloadJSON)
	}
	return s.toWebhook(instanceID, target.Webhook, task, payloadJSON)
}

func (s *Service) toFunction(ref string, task Envelope, payloadJSON string) Outcome {
	inst, name := SplitRef(ref)
	if s.reg.Get("functions", inst) == nil {
		return Outcome{Status: "error", Error: fmt.Sprintf("no functions instance '%s'", inst), Permanent: true}
	}
	if s.mux == nil {
		return Outcome{Status: "error", Error: "functions are not served here", Permanent: true}
	}
	req := httptest.NewRequest(http.MethodPost, "/fn/"+url.PathEscape(inst)+"/"+url.PathEscape(name), bytes.NewReader(DeliveryBody(task, payloadJSON)))
	// Marked as a trigger, and carrying the task's depth: a write the function makes fires its
	// own triggers one deeper, which is what cuts a function that writes what triggers it.
	req = common.WithTrigger(req, "task")
	req = req.WithContext(common.WithTriggerDepth(req.Context(), task.Depth))
	setTaskHeaders(req.Header, task)
	req.Header.Set("authorization", "Bearer emulator-internal")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.mux.ServeHTTP(rec, req)
	}()
	wait, timedOut := s.deadline(task)
	select {
	case <-done:
	case <-time.After(wait):
		return Outcome{Status: "timeout", Error: timedOut}
	}
	result := resultOf(rec.Body.Bytes())
	control := ReadTargetControl(rec.Header(), s.now())
	status := fmt.Sprintf("http:%d", rec.Code)
	if rec.Code >= 200 && rec.Code < 300 {
		return Outcome{OK: true, Status: status, Result: result, Control: control}
	}
	// A 404 with no control headers is "no such function"; one the function returned itself
	// carries its own control headers if it wants different treatment.
	if rec.Code == 404 && control == nil {
		return Outcome{Status: "http:404", Error: fmt.Sprintf("function '%s' not found", name), Permanent: true, Result: result}
	}
	why := fmt.Sprintf("function answered %d", rec.Code)
	if result != nil {
		why, _ = clipBytes(*result, 500)
	}
	// A redirect is not followed here either: a task is not a browser, and retrying it returns the
	// same redirect. Same rule as a webhook.
	return Outcome{Status: status, Error: why, Permanent: permanentStatus(rec.Code) || (rec.Code >= 300 && rec.Code < 400), Result: result, Control: control}
}

func (s *Service) toChannel(ref string, task Envelope, payloadJSON string) Outcome {
	inst, name := SplitRef(ref)
	in := s.reg.Get("channel", inst)
	if in == nil {
		return Outcome{Status: "error", Error: fmt.Sprintf("no channel instance '%s'", inst), Permanent: true}
	}
	if s.hub == nil {
		return Outcome{Status: "error", Error: "channels are not served here", Permanent: true}
	}
	t, _ := json.Marshal(task)
	data := []byte(`{"type":"task","task":` + string(t) + `,"payload":` + payloadJSON + `}`)
	framed, _ := json.Marshal(map[string]any{"channel": name, "data": json.RawMessage(data), "ts": s.now()})
	if len(framed) > channel.MaxMessageBytes {
		return Outcome{Status: "error", Permanent: true, Error: fmt.Sprintf("the task is %d KB framed; a channel message is at most %d KB",
			(len(framed)+1023)/1024, channel.MaxMessageBytes/1024)}
	}
	n, err := s.hub.Publish(in.ID, name, data)
	if err != nil {
		return Outcome{Status: "error", Error: err.Error()}
	}
	return Outcome{OK: true, Status: fmt.Sprintf("published:%d", n)}
}

func (s *Service) toWebhook(instanceID, target string, task Envelope, payloadJSON string) Outcome {
	key, err := s.store.WebhookSecret(instanceID)
	if err != nil || key == "" {
		// Nothing is sent unsigned. A key that could not be read is a retry, not a delivery.
		return Outcome{Status: "error", Error: "signing key unavailable"}
	}
	body := DeliveryBody(task, payloadJSON)
	ts := time.Now().UnixMilli()
	wait, timedOut := s.deadline(task)
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return Outcome{Status: "error", Error: err.Error()}
	}
	setTaskHeaders(req.Header, task)
	req.Header.Set(SignatureHeader, SignWebhook(key, ts, body))
	req.Header.Set(TimestampHeader, strconv.FormatInt(ts, 10))
	res, err := s.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ue) && ue.Timeout()) {
			return Outcome{Status: "timeout", Error: timedOut}
		}
		return Outcome{Status: "error", Error: err.Error()}
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, MaxResultBytes+4))
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
	result := resultOf(raw)
	control := ReadTargetControl(res.Header, s.now())
	status := fmt.Sprintf("http:%d", res.StatusCode)
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return Outcome{OK: true, Status: status, Result: result, Control: control}
	}
	why := fmt.Sprintf("receiver answered %d", res.StatusCode)
	if result != nil {
		why, _ = clipBytes(*result, 500)
	}
	// A redirect is not followed: the URL is the receiver, and a 3xx is a misconfiguration.
	return Outcome{Status: status, Error: why, Permanent: permanentStatus(res.StatusCode) || (res.StatusCode >= 300 && res.StatusCode < 400),
		Result: result, Control: control}
}

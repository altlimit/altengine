package mailbox

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
)

// MIME, both directions, with the standard library (plus x/text for legacy charsets, already in
// the module graph). Parsing is tolerant the way a mail client has to be: a part that will not
// decode is kept as bytes rather than failing the message, because a malformed message from the
// internet is still a message someone is waiting for.

// ParsedAttachment is one attachment as found in a message.
type ParsedAttachment struct {
	Filename    string
	MimeType    string
	Content     []byte
	ContentID   string
	Disposition string
}

// ParsedEmail is what ingest reads out of a raw message.
type ParsedEmail struct {
	From, To, Cc, ReplyTo []AddressValue
	Subject               string
	Text, HTML            *string
	MessageID             string
	InReplyTo             string
	References            string
	AuthResults           []string // every Authentication-Results value, top first
	Attachments           []ParsedAttachment
}

const maxNesting = 50

func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	cs := strings.ToLower(strings.TrimSpace(charset))
	if cs == "" || cs == "utf-8" || cs == "utf8" || cs == "us-ascii" || cs == "ascii" {
		return input, nil
	}
	enc, err := htmlindex.Get(cs)
	if err != nil {
		return nil, fmt.Errorf("unknown charset %q", charset)
	}
	return enc.NewDecoder().Reader(input), nil
}

var wordDecoder = &mime.WordDecoder{CharsetReader: charsetReader}

func decodeHeader(v string) string {
	if d, err := wordDecoder.DecodeHeader(v); err == nil {
		v = d
	}
	return strings.ToValidUTF8(v, "�")
}

func decodeText(b []byte, charset string) string {
	r, err := charsetReader(charset, bytes.NewReader(b))
	if err == nil {
		if out, err := io.ReadAll(r); err == nil {
			b = out
		}
	}
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

// parseAddressList reads an address header, tolerating what net/mail refuses: a list that does
// not parse whole is retried one entry at a time, and an entry that still does not parse but
// carries an @ is kept as written.
func parseAddressList(v string) []AddressValue {
	out := []AddressValue{}
	if strings.TrimSpace(v) == "" {
		return out
	}
	p := &mail.AddressParser{WordDecoder: wordDecoder}
	if list, err := p.ParseList(v); err == nil {
		for _, a := range list {
			out = append(out, AddressValue{Address: a.Address, Name: a.Name})
		}
	} else {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if a, err := p.Parse(part); err == nil {
				out = append(out, AddressValue{Address: a.Address, Name: a.Name})
			} else if strings.Contains(part, "@") {
				out = append(out, AddressValue{Address: strings.Trim(part, "<> \t"), Name: ""})
			}
		}
	}
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

func transferDecode(enc string, body []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "base64":
		clean := bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, body)
		clean = bytes.TrimRight(clean, "=")
		out := make([]byte, base64.RawStdEncoding.DecodedLen(len(clean)))
		n, err := base64.RawStdEncoding.Decode(out, clean)
		if err != nil {
			return out[:n]
		}
		return out[:n]
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err != nil {
			return out
		}
		return out
	}
	return body
}

// ParseEmail parses a raw RFC 5322 message.
func ParseEmail(raw []byte) *ParsedEmail {
	e := &ParsedEmail{From: []AddressValue{}, To: []AddressValue{}, Cc: []AddressValue{}, ReplyTo: []AddressValue{}}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		// No header block we can read: the whole thing is the body.
		t := strings.ToValidUTF8(string(raw), "�")
		e.Text = &t
		return e
	}
	h := textproto.MIMEHeader(msg.Header)
	e.From = parseAddressList(h.Get("From"))
	e.To = parseAddressList(h.Get("To"))
	e.Cc = parseAddressList(h.Get("Cc"))
	e.ReplyTo = parseAddressList(h.Get("Reply-To"))
	e.Subject = decodeHeader(h.Get("Subject"))
	e.MessageID = strings.TrimSpace(h.Get("Message-Id"))
	e.InReplyTo = strings.TrimSpace(h.Get("In-Reply-To"))
	e.References = strings.TrimSpace(h.Get("References"))
	e.AuthResults = h.Values("Authentication-Results")
	body, _ := io.ReadAll(msg.Body)
	e.walk(h, body, 0)
	return e
}

func (e *ParsedEmail) walk(h textproto.MIMEHeader, body []byte, depth int) {
	ct := h.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil || mediaType == "" {
		mediaType, params = "text/plain", map[string]string{}
	}
	if strings.HasPrefix(mediaType, "multipart/") && depth < maxNesting {
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			p, err := mr.NextRawPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			e.walk(p.Header, b, depth+1)
		}
		return
	}
	content := transferDecode(h.Get("Content-Transfer-Encoding"), body)
	disp, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	filename := dparams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	filename = decodeHeader(filename)
	isBody := disp != "attachment" && (mediaType == "text/plain" || mediaType == "text/html")
	if isBody {
		s := decodeText(content, params["charset"])
		if mediaType == "text/plain" && e.Text == nil {
			e.Text = &s
			return
		}
		if mediaType == "text/html" && e.HTML == nil {
			e.HTML = &s
			return
		}
		if mediaType == "text/plain" {
			joined := *e.Text + "\n" + s
			e.Text = &joined
			return
		}
	}
	e.Attachments = append(e.Attachments, ParsedAttachment{
		Filename:    filename,
		MimeType:    mediaType,
		Content:     content,
		ContentID:   strings.TrimSpace(h.Get("Content-Id")),
		Disposition: disp,
	})
}

// --- building ------------------------------------------------------------------------------

// OutAttachment is a file to put in a built message.
type OutAttachment struct {
	Filename    string
	ContentType string
	Content     []byte
}

// OutMessage is everything BuildMessage writes.
type OutMessage struct {
	From        AddressValue
	To, Cc      []AddressValue
	Subject     string
	Text, HTML  string
	MessageID   string
	InReplyTo   string
	References  string
	Date        time.Time
	Attachments []OutAttachment
	// Headers are extra header lines written first, verbatim (no CR or LF in them).
	Headers [][2]string
}

func addrList(list []AddressValue) string {
	parts := make([]string, 0, len(list))
	for _, a := range list {
		parts = append(parts, (&mail.Address{Name: a.Name, Address: a.Address}).String())
	}
	return strings.Join(parts, ", ")
}

func headerSafe(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

func encodeWord(s string) string {
	for _, r := range s {
		if r >= 0x80 || r < 0x20 {
			return mime.QEncoding.Encode("utf-8", s)
		}
	}
	return s
}

func writeTextPart(w io.Writer, text, contentType string) {
	fmt.Fprintf(w, "Content-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", contentType)
	qp := quotedprintable.NewWriter(w)
	_, _ = qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")))
	_ = qp.Close()
	_, _ = io.WriteString(w, "\r\n")
}

func partHeader(contentType string, extra map[string]string) textproto.MIMEHeader {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", contentType)
	for k, v := range extra {
		h.Set(k, v)
	}
	return h
}

// writeAlternative writes a Content-Type header, a blank line and the body: a text/html
// alternative, or the one part there is.
func writeAlternative(buf *bytes.Buffer, text, html string) {
	switch {
	case text != "" && html != "":
		alt := multipart.NewWriter(buf)
		fmt.Fprintf(buf, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", alt.Boundary())
		for _, p := range []struct{ body, ct string }{{text, "text/plain"}, {html, "text/html"}} {
			pw, _ := alt.CreatePart(partHeader(p.ct+"; charset=utf-8", map[string]string{"Content-Transfer-Encoding": "quoted-printable"}))
			qp := quotedprintable.NewWriter(pw)
			_, _ = qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(p.body, "\r\n", "\n"), "\n", "\r\n")))
			_ = qp.Close()
		}
		_ = alt.Close()
	case html != "":
		writeTextPart(buf, html, "text/html")
	default:
		writeTextPart(buf, text, "text/plain")
	}
}

// BuildMessage renders a message as RFC 5322 bytes with CRLF line endings.
func BuildMessage(m OutMessage) []byte {
	var buf bytes.Buffer
	for _, h := range m.Headers {
		fmt.Fprintf(&buf, "%s: %s\r\n", h[0], headerSafe(h[1]))
	}
	date := m.Date
	if date.IsZero() {
		date = time.Now()
	}
	fmt.Fprintf(&buf, "From: %s\r\n", addrList([]AddressValue{m.From}))
	if len(m.To) > 0 {
		fmt.Fprintf(&buf, "To: %s\r\n", addrList(m.To))
	}
	if len(m.Cc) > 0 {
		fmt.Fprintf(&buf, "Cc: %s\r\n", addrList(m.Cc))
	}
	fmt.Fprintf(&buf, "Subject: %s\r\n", encodeWord(headerSafe(m.Subject)))
	fmt.Fprintf(&buf, "Date: %s\r\n", date.Format(time.RFC1123Z))
	if m.MessageID != "" {
		fmt.Fprintf(&buf, "Message-ID: %s\r\n", headerSafe(m.MessageID))
	}
	if m.InReplyTo != "" {
		fmt.Fprintf(&buf, "In-Reply-To: %s\r\n", headerSafe(m.InReplyTo))
	}
	if m.References != "" {
		fmt.Fprintf(&buf, "References: %s\r\n", headerSafe(m.References))
	}
	buf.WriteString("MIME-Version: 1.0\r\n")
	if len(m.Attachments) == 0 {
		writeAlternative(&buf, m.Text, m.HTML)
		return buf.Bytes()
	}
	mixed := multipart.NewWriter(&buf)
	fmt.Fprintf(&buf, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", mixed.Boundary())
	if m.Text != "" || m.HTML != "" {
		var inner bytes.Buffer
		writeAlternative(&inner, m.Text, m.HTML)
		// The alternative's own header lines become the part's header.
		hdr, body, _ := bytes.Cut(inner.Bytes(), []byte("\r\n\r\n"))
		ph := textproto.MIMEHeader{}
		for _, line := range strings.Split(string(hdr), "\r\n") {
			if k, v, ok := strings.Cut(line, ": "); ok {
				ph.Set(k, v)
			}
		}
		pw, _ := mixed.CreatePart(ph)
		_, _ = pw.Write(body)
	}
	for _, a := range m.Attachments {
		ct := a.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		name := a.Filename
		if name == "" {
			name = "attachment"
		}
		ph := textproto.MIMEHeader{}
		typ := mime.FormatMediaType(ct, map[string]string{"name": name})
		if typ == "" {
			typ = mime.FormatMediaType("application/octet-stream", map[string]string{"name": name})
		}
		ph.Set("Content-Type", typ)
		ph.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		ph.Set("Content-Transfer-Encoding", "base64")
		pw, _ := mixed.CreatePart(ph)
		enc := base64.StdEncoding.EncodeToString(a.Content)
		for len(enc) > 76 {
			_, _ = io.WriteString(pw, enc[:76]+"\r\n")
			enc = enc[76:]
		}
		_, _ = io.WriteString(pw, enc+"\r\n")
	}
	_ = mixed.Close()
	return buf.Bytes()
}

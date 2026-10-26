package mailbox

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A minimal ESMTP listener, so a real app's mail library (or swaks) can deliver to the emulator.
// Off unless `altengine dev --smtp` is passed. It speaks just enough of RFC 5321 for a client to
// hand over a message — EHLO/HELO, MAIL FROM, RCPT TO, DATA, RSET, NOOP, QUIT — and feeds what it
// receives into the same inbound path as everything else.
//
// Refusals happen where a real receiving server makes them: an unknown address, a disabled or
// unregistered one, or a mailbox over its day's cap is refused at RCPT TO with the hosted SMTP
// text; a message over the size limit is refused at the end of DATA (or at MAIL FROM when SIZE
// says so up front). No TLS, no AUTH: it binds to loopback and is for local testing.

const (
	smtpMaxRecipients = 100
	smtpIdleTimeout   = 5 * time.Minute
	smtpMaxLine       = 4096
)

// SMTPServer is the listener.
type SMTPServer struct {
	svc           *Service
	ln            net.Listener
	authenticated bool
	hostname      string
	wg            sync.WaitGroup
	closed        chan struct{}
}

// ListenSMTP binds addr. authenticated says whether mail received this way counts as coming from
// an authenticated sender (see authStamp) — what lets reply mode be tested end to end.
func (s *Service) ListenSMTP(addr string, authenticated bool) (*SMTPServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &SMTPServer{svc: s, ln: ln, authenticated: authenticated, hostname: s.domain, closed: make(chan struct{})}, nil
}

// Addr is the bound address.
func (m *SMTPServer) Addr() net.Addr { return m.ln.Addr() }

// Serve accepts connections until Close.
func (m *SMTPServer) Serve() {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			select {
			case <-m.closed:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			log.Printf("[smtp] accept: %v", err)
			return
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.handle(c)
		}()
	}
}

// Close stops accepting and waits for sessions in flight.
func (m *SMTPServer) Close() error {
	close(m.closed)
	err := m.ln.Close()
	m.wg.Wait()
	return err
}

var (
	mailFromCmdRe = regexp.MustCompile(`(?i)^FROM:\s*<([^>]*)>(.*)$`)
	rcptToCmdRe   = regexp.MustCompile(`(?i)^TO:\s*<([^>]+)>`)
	sizeParamRe   = regexp.MustCompile(`(?i)\bSIZE=(\d+)`)
)

type smtpRcpt struct {
	address string
	r       Recipient
}

func (m *SMTPServer) handle(c net.Conn) {
	defer c.Close()
	tp := textproto.NewConn(c)
	reply := func(line string) bool {
		_ = c.SetWriteDeadline(time.Now().Add(time.Minute))
		return tp.PrintfLine("%s", line) == nil
	}
	if !reply(fmt.Sprintf("220 %s ESMTP altengine emulator", m.hostname)) {
		return
	}
	var (
		greeted bool
		from    string
		inTx    bool
		rcpts   []smtpRcpt
	)
	reset := func() { from, inTx, rcpts = "", false, nil }
	for {
		_ = c.SetReadDeadline(time.Now().Add(smtpIdleTimeout))
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		if len(line) > smtpMaxLine {
			reply("500 5.5.2 line too long")
			continue
		}
		verb, arg, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		arg = strings.TrimSpace(arg)
		switch verb {
		case "EHLO":
			greeted = true
			reset()
			_ = tp.PrintfLine("250-%s greets %s", m.hostname, arg)
			_ = tp.PrintfLine("250-SIZE %d", MaxMessageBytes)
			_ = tp.PrintfLine("250-8BITMIME")
			reply("250 SMTPUTF8")
		case "HELO":
			greeted = true
			reset()
			reply("250 " + m.hostname)
		case "MAIL":
			if !greeted {
				reply("503 5.5.1 say EHLO first")
				continue
			}
			if inTx {
				reply("503 5.5.1 nested MAIL command")
				continue
			}
			mm := mailFromCmdRe.FindStringSubmatch(arg)
			if mm == nil {
				reply("501 5.5.4 syntax: MAIL FROM:<address>")
				continue
			}
			if sm := sizeParamRe.FindStringSubmatch(mm[2]); sm != nil {
				if n, _ := strconv.ParseInt(sm[1], 10, 64); n > MaxMessageBytes {
					reply(rejectTooLarge)
					continue
				}
			}
			from, inTx = mm[1], true
			reply("250 2.1.0 OK")
		case "RCPT":
			if !inTx {
				reply("503 5.5.1 need MAIL first")
				continue
			}
			rm := rcptToCmdRe.FindStringSubmatch(arg)
			if rm == nil {
				reply("501 5.5.4 syntax: RCPT TO:<address>")
				continue
			}
			if len(rcpts) >= smtpMaxRecipients {
				reply("452 4.5.3 too many recipients")
				continue
			}
			_, r, err := m.svc.AdmitAddress(rm[1], 0)
			if err != nil {
				var rj *Rejection
				if errors.As(err, &rj) {
					reply(rj.SMTP)
				} else {
					reply("451 4.3.0 local error")
				}
				continue
			}
			rcpts = append(rcpts, smtpRcpt{address: rm[1], r: r})
			reply("250 2.1.5 OK")
		case "DATA":
			if !inTx || len(rcpts) == 0 {
				reply("503 5.5.1 need RCPT first")
				continue
			}
			if !reply("354 end data with <CR><LF>.<CR><LF>") {
				return
			}
			_ = c.SetReadDeadline(time.Now().Add(smtpIdleTimeout))
			raw, tooBig, err := readData(tp.DotReader())
			if err != nil {
				return
			}
			if tooBig {
				reply(rejectTooLarge)
				reset()
				continue
			}
			reply(m.deliver(from, rcpts, raw))
			reset()
		case "RSET":
			reset()
			reply("250 2.0.0 OK")
		case "NOOP":
			reply("250 2.0.0 OK")
		case "VRFY":
			reply("252 2.5.0 cannot verify, but will accept")
		case "QUIT":
			reply("221 2.0.0 bye")
			return
		default:
			reply("502 5.5.2 command not recognized")
		}
	}
}

// readData reads a dot-terminated body, draining but not keeping anything past the size limit.
func readData(r io.Reader) ([]byte, bool, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxMessageBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > MaxMessageBytes {
		_, err := io.Copy(io.Discard, r)
		return nil, true, err
	}
	// The dot reader yields LF line endings; a message stored as received keeps CRLF.
	return []byte(strings.ReplaceAll(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n", "\r\n")), false, nil
}

// deliver ingests the message for each admitted recipient and answers DATA once for all of them.
func (m *SMTPServer) deliver(from string, rcpts []smtpRcpt, raw []byte) string {
	var ids []string
	for _, rc := range rcpts {
		inst := m.svc.instanceBySlug(rc.r.Slug)
		if inst == nil {
			continue // deleted between RCPT and DATA
		}
		rec, err := m.svc.Ingest(inst, Local{Local: rc.r.Local, Tag: rc.r.Tag}, from, raw, m.authenticated)
		if err != nil {
			log.Printf("[smtp] delivering to %s: %v", rc.address, err)
			return "451 4.3.0 local error, try again later"
		}
		ids = append(ids, rec.ID)
	}
	if len(ids) == 0 {
		return rejectNoAddress
	}
	return "250 2.0.0 OK queued as " + strings.Join(ids, ",")
}

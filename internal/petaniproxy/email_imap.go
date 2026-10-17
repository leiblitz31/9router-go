package petaniproxy

// IMAPEmailClient is the production EmailClient implementation used by
// grok_farm to poll verification/OTP emails. It is built on
// github.com/emersion/go-imap (v1) for the wire protocol and
// github.com/emersion/go-message/mail for MIME parsing.
//
// The go-imap client is not safe for concurrent use, so every public method
// serializes through an internal mutex. All blocking IMAP calls run in a
// helper goroutine selected against ctx.Done so callers can cancel.

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
	"golang.org/x/text/encoding/charmap"
)

// imapDialTimeout bounds the initial TCP/TLS handshake of the constructor.
const imapDialTimeout = 30 * time.Second

// IMAPEmailClient implements EmailClient over a single IMAP connection with
// INBOX selected read-write.
type IMAPEmailClient struct {
	mu     sync.Mutex
	client *imapclient.Client
}

// NewIMAPEmailClient dials addr (e.g. "imap.gmail.com:993"), logs in and
// selects INBOX. When useTLS is true the connection is wrapped in TLS
// (implicit TLS, the standard for port 993). Credentials come only from the
// parameters — nothing is hardcoded or read from the environment here.
func NewIMAPEmailClient(addr string, username, password string, useTLS bool) (*IMAPEmailClient, error) {
	if strings.TrimSpace(addr) == "" {
		return nil, errors.New("imap: addr is empty")
	}
	if strings.TrimSpace(username) == "" {
		return nil, errors.New("imap: username is empty")
	}
	if strings.TrimSpace(password) == "" {
		return nil, errors.New("imap: password is empty")
	}

	dialer := &net.Dialer{Timeout: imapDialTimeout}
	var (
		conn net.Conn
		err  error
	)
	if useTLS {
		host, _, splitErr := net.SplitHostPort(addr)
		if splitErr != nil {
			return nil, fmt.Errorf("imap: invalid addr %q: %w", addr, splitErr)
		}
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("imap: dial %s: %w", addr, err)
	}

	c, err := imapclient.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("imap: handshake %s: %w", addr, err)
	}
	fail := func(format string, args ...any) (*IMAPEmailClient, error) {
		_ = c.Logout()
		return nil, fmt.Errorf(format, args...)
	}
	if err := c.Login(username, password); err != nil {
		return fail("imap: login: %w", err)
	}
	if _, err := c.Select("INBOX", false); err != nil {
		return fail("imap: select INBOX: %w", err)
	}
	return &IMAPEmailClient{client: c}, nil
}

// imapRun executes a blocking IMAP call and returns early when ctx is done.
// The mutex must already be held by the caller.
func imapRun[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := fn()
		done <- result{v: v, err: err}
	}()
	select {
	case <-ctx.Done():
		var zero T
		return zero, fmt.Errorf("imap: %w", ctx.Err())
	case r := <-done:
		return r.v, r.err
	}
}

// Search returns message UIDs (as decimal strings) matching the simple
// criteria language: "UNSEEN", "UNSEEN FROM <addr>", "UNSEEN SUBJECT <text>".
// SUBJECT takes the remainder of the string so it may contain spaces.
func (e *IMAPEmailClient) Search(ctx context.Context, criteria string) ([]string, error) {
	crit, err := parseIMAPSearchCriteria(criteria)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	uids, err := imapRun(ctx, func() ([]uint32, error) {
		return e.client.UidSearch(crit)
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(uids))
	for _, u := range uids {
		ids = append(ids, strconv.FormatUint(uint64(u), 10))
	}
	return ids, nil
}

// parseIMAPSearchCriteria maps the simple criteria language onto
// imap.SearchCriteria. Pure function — no network.
func parseIMAPSearchCriteria(s string) (*imap.SearchCriteria, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("imap: empty search criteria")
	}
	crit := imap.NewSearchCriteria()
	toks := strings.Fields(s)
	i := 0
	for i < len(toks) {
		switch strings.ToUpper(toks[i]) {
		case "UNSEEN":
			crit.WithoutFlags = append(crit.WithoutFlags, imap.SeenFlag)
			i++
		case "SEEN":
			crit.WithFlags = append(crit.WithFlags, imap.SeenFlag)
			i++
		case "FROM":
			i++
			if i >= len(toks) {
				return nil, errors.New("imap: FROM needs an address")
			}
			imapSearchHeader(crit, "From", toks[i])
			i++
		case "SUBJECT":
			i++
			if i >= len(toks) {
				return nil, errors.New("imap: SUBJECT needs text")
			}
			imapSearchHeader(crit, "Subject", strings.Join(toks[i:], " "))
			i = len(toks)
		default:
			return nil, fmt.Errorf("imap: unsupported search token %q", toks[i])
		}
	}
	return crit, nil
}

func imapSearchHeader(crit *imap.SearchCriteria, key, value string) {
	if crit.Header == nil {
		crit.Header = make(textproto.MIMEHeader)
	}
	crit.Header.Add(key, value)
}

// Fetch downloads ENVELOPE + full body (BODY.PEEK so the \Seen flag is not
// touched) for the UID in id and parses it into a GrokEmailMessage.
func (e *IMAPEmailClient) Fetch(ctx context.Context, id string) (*GrokEmailMessage, error) {
	uid64, err := strconv.ParseUint(strings.TrimSpace(id), 10, 32)
	if err != nil {
		return nil, fmt.Errorf("imap: invalid message id %q: %w", id, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return imapRun(ctx, func() (*GrokEmailMessage, error) {
		return e.fetchLocked(uint32(uid64))
	})
}

func (e *IMAPEmailClient) fetchLocked(uid uint32) (*GrokEmailMessage, error) {
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)
	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}

	ch := make(chan *imap.Message, 8)
	fetchErr := make(chan error, 1)
	go func() { fetchErr <- e.client.UidFetch(seqSet, items, ch) }()

	var first *imap.Message
	for m := range ch {
		if first == nil {
			first = m
		}
	}
	if err := <-fetchErr; err != nil {
		return nil, fmt.Errorf("imap: fetch uid %d: %w", uid, err)
	}
	if first == nil {
		return nil, fmt.Errorf("imap: message uid %d not found", uid)
	}

	raw := first.GetBody(section)
	if raw == nil {
		return nil, fmt.Errorf("imap: message uid %d has no body", uid)
	}
	data, err := io.ReadAll(raw)
	if err != nil {
		return nil, fmt.Errorf("imap: read body uid %d: %w", uid, err)
	}
	return parseGrokEmailRaw(data)
}

// parseGrokEmailRaw parses a full RFC822 message into a GrokEmailMessage.
// Pure function — no network. Body prefers text/plain, falling back to
// stripped text/html.
func parseGrokEmailRaw(data []byte) (*GrokEmailMessage, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("imap: empty message")
	}
	mr, err := mail.CreateReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("imap: parse message: %w", err)
	}
	out := &GrokEmailMessage{
		To:          decodeIMAPHeaderValue(mr.Header.Get("To")),
		DeliveredTo: decodeIMAPHeaderValue(mr.Header.Get("Delivered-To")),
		Subject:     decodeIMAPHeaderValue(mr.Header.Get("Subject")),
		From:        decodeIMAPHeaderValue(mr.Header.Get("From")),
	}

	var plain, html strings.Builder
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("imap: read part: %w", err)
		}
		body, err := io.ReadAll(p.Body)
		if err != nil {
			return nil, fmt.Errorf("imap: read part body: %w", err)
		}
		// RFC 2045 §5.2: no Content-Type header defaults to
		// text/plain; charset=us-ascii.
		mediaType := imapMediaType(p.Header.Get("Content-Type"))
		if mediaType == "" {
			mediaType = "text/plain"
		}
		switch mediaType {
		case "text/plain":
			plain.WriteString(imapDecodeBody(body, p.Header.Get("Content-Type")))
		case "text/html":
			html.WriteString(imapDecodeBody(body, p.Header.Get("Content-Type")))
		}
	}

	switch {
	case plain.Len() > 0:
		out.Body = strings.TrimSpace(plain.String())
	case html.Len() > 0:
		out.Body = strings.TrimSpace(stripIMAPHTML(html.String()))
	}
	return out, nil
}

// decodeIMAPHeaderValue decodes RFC2047 encoded words (=?UTF-8?...?=) found in
// headers; returns the input unchanged when undecodable.
func decodeIMAPHeaderValue(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || !strings.Contains(s, "=?") {
		return s
	}
	dec := new(mime.WordDecoder)
	if d, err := dec.DecodeHeader(s); err == nil {
		return d
	}
	return s
}

func imapMediaType(contentType string) string {
	mt := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	return strings.TrimSpace(mt)
}

func imapCharset(contentType string) string {
	ct := strings.ToLower(contentType)
	for _, part := range strings.Split(ct, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "charset=") {
			return strings.Trim(strings.TrimPrefix(part, "charset="), `"' `)
		}
	}
	return ""
}

// imapDecodeBody converts part bytes to UTF-8 using the declared charset
// (transfer encoding is already handled by go-message). Unknown charsets
// fall back to sanitizing invalid UTF-8.
func imapDecodeBody(data []byte, contentType string) string {
	switch imapCharset(contentType) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return string(data)
	case "iso-8859-1", "latin1":
		dec, err := charmap.ISO8859_1.NewDecoder().Bytes(data)
		if err == nil {
			return string(dec)
		}
	case "windows-1252", "cp1252":
		dec, err := charmap.Windows1252.NewDecoder().Bytes(data)
		if err == nil {
			return string(dec)
		}
	}
	return strings.ToValidUTF8(string(data), "")
}

var imapHTMLTagRe = regexp.MustCompile(`(?s)<[^>]*>`)

// stripIMAPHTML is a deliberately simple HTML→text fallback for OTP mails
// (verification codes live in plain text runs, not markup).
func stripIMAPHTML(s string) string {
	s = imapHTMLTagRe.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	return strings.Join(strings.Fields(s), " ")
}

// MarkRead sets the \Seen flag on the message UID.
func (e *IMAPEmailClient) MarkRead(ctx context.Context, id string) error {
	uid64, err := strconv.ParseUint(strings.TrimSpace(id), 10, 32)
	if err != nil {
		return fmt.Errorf("imap: invalid message id %q: %w", id, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err = imapRun(ctx, func() (struct{}, error) {
		seqSet := new(imap.SeqSet)
		seqSet.AddNum(uint32(uid64))
		op := imap.FormatFlagsOp(imap.AddFlags, true)
		flags := []interface{}{imap.SeenFlag}
		if err := e.client.UidStore(seqSet, op, flags, nil); err != nil {
			return struct{}{}, fmt.Errorf("imap: mark read uid %d: %w", uid64, err)
		}
		return struct{}{}, nil
	})
	return err
}

// Close logs out of the IMAP session and terminates the connection.
func (e *IMAPEmailClient) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.client.Logout(); err != nil {
		return fmt.Errorf("imap: logout: %w", err)
	}
	return nil
}

// Compile-time check that IMAPEmailClient satisfies the seam.
var _ EmailClient = (*IMAPEmailClient)(nil)

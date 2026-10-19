package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const resendEmailEndpoint = "https://api.resend.com/emails"

// EmailMessage is one message to one recipient. Exactly one of HTML or Text
// is the body. IdempotencyKey identifies retries of the same logical message.
type EmailMessage struct {
	From           string
	To             string
	Subject        string
	HTML           string
	Text           string
	IdempotencyKey string
}

// EmailSender delivers a single message. A *rejectedEmailError means the
// message will never be accepted as is; an *unknownEmailOutcomeError means it
// may already have been accepted. Any other error is safe to retry.
type EmailSender interface {
	SendEmail(ctx context.Context, m EmailMessage) error
}

// EmailTransport selects how every API email leaves the process. The zero
// value sends through Resend. A non-empty SMTPAddr sends through that SMTP
// server instead and never falls back to Resend.
type EmailTransport struct {
	SMTPAddr string
}

// Sender returns the sender for a feature that owns resendAPIKey, or nil when
// the selected transport is Resend and the feature has no key.
func (t EmailTransport) Sender(resendAPIKey string) EmailSender {
	if t.SMTPAddr != "" {
		return &SMTPEmailSender{addr: t.SMTPAddr, timeout: 10 * time.Second}
	}
	if resendAPIKey == "" {
		return nil
	}
	return NewResendEmailSender(resendAPIKey)
}

type rejectedEmailError struct{ err error }

func (e *rejectedEmailError) Error() string { return e.err.Error() }
func (e *rejectedEmailError) Unwrap() error { return e.err }

// unknownEmailOutcomeError reports that delivery may have been accepted
// despite the error (for example, a timeout after submission). Callers that
// promise at-most-once delivery must not retry it automatically.
type unknownEmailOutcomeError struct{ err error }

func (e *unknownEmailOutcomeError) Error() string { return e.err.Error() }
func (e *unknownEmailOutcomeError) Unwrap() error { return e.err }

// ResendEmailSender delivers through the Resend HTTP API.
type ResendEmailSender struct {
	apiKey   string
	endpoint string
	client   *http.Client
}

func NewResendEmailSender(apiKey string) *ResendEmailSender {
	return &ResendEmailSender{
		apiKey:   apiKey,
		endpoint: resendEmailEndpoint,
		client: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (s *ResendEmailSender) SendEmail(ctx context.Context, m EmailMessage) error {
	payload := map[string]any{"from": m.From, "to": []string{m.To}, "subject": m.Subject}
	if m.HTML != "" {
		payload["html"] = m.HTML
	} else {
		payload["text"] = m.Text
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var gettingConn, gotConn atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn: func(string) { gettingConn.Store(true) },
		GotConn: func(httptrace.GotConnInfo) { gotConn.Store(true) },
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	if m.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", m.IdempotencyKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		// Failure to obtain a connection (including DNS, dial, and TLS)
		// precedes submission. Once connected, even a partial write may
		// have reached the provider. Untraced transports remain ambiguous.
		if gettingConn.Load() && !gotConn.Load() {
			return err
		}
		return &unknownEmailOutcomeError{err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	var providerError struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(snippet, &providerError)
	err = fmt.Errorf("resend returned %d (%s)", resp.StatusCode, providerError.Name)
	// Keep request timeouts and recoverable provider configuration errors
	// (bad key, unverified domain, malformed from address) retryable.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 &&
		resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden &&
		resp.StatusCode != http.StatusRequestTimeout &&
		resp.StatusCode != http.StatusTooManyRequests && providerError.Name != "invalid_from_address" {
		return &rejectedEmailError{err: err}
	}
	return err
}

// SMTPEmailSender relays through an SMTP server without authentication or
// TLS, which suits a relay or capture server on a trusted local network.
type SMTPEmailSender struct {
	addr    string
	timeout time.Duration
}

func (s *SMTPEmailSender) SendEmail(ctx context.Context, m EmailMessage) error {
	from, to, data, err := smtpMessage(m, time.Now())
	if err != nil {
		return err
	}
	dialer := net.Dialer{Timeout: s.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(s.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(s.addr)
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer client.Close()
	// Nothing is submitted until the server answers the end of DATA, so
	// failures before then are definitive. Greeting and sender failures are
	// server or configuration problems, retryable once fixed; only replies
	// about this recipient or this message can reject it permanently.
	if err := client.Mail(from); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return smtpReplyError(err)
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		var reply *textproto.Error
		if errors.As(err, &reply) {
			return smtpReplyError(err)
		}
		return &unknownEmailOutcomeError{err: err}
	}
	_ = client.Quit()
	return nil
}

// smtpReplyError classifies a server reply: 5xx is a permanent rejection,
// anything else (4xx, I/O before submission) is retryable.
func smtpReplyError(err error) error {
	var reply *textproto.Error
	if errors.As(err, &reply) && reply.Code >= 500 {
		return &rejectedEmailError{err: err}
	}
	return err
}

// smtpMessage renders m as an RFC 5322 message and returns the envelope
// addresses. Addresses are parsed so header values cannot carry line breaks.
// An invalid sender is a configuration error and stays retryable; an invalid
// recipient is a permanent rejection.
func smtpMessage(m EmailMessage, now time.Time) (string, string, []byte, error) {
	from, err := mail.ParseAddress(m.From)
	if err != nil {
		return "", "", nil, fmt.Errorf("invalid from address: %w", err)
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return "", "", nil, &rejectedEmailError{err: fmt.Errorf("invalid recipient address: %w", err)}
	}
	contentType, body := "text/plain", m.Text
	if m.HTML != "" {
		contentType, body = "text/html", m.HTML
	}
	messageID := uuid.NewString()
	if m.IdempotencyKey != "" {
		messageID = fmt.Sprintf("%x", sha256.Sum256([]byte(m.IdempotencyKey)))
	}
	domain := from.Address[strings.LastIndex(from.Address, "@")+1:]
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", from.String())
	fmt.Fprintf(&buf, "To: %s\r\n", to.String())
	fmt.Fprintf(&buf, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", m.Subject))
	fmt.Fprintf(&buf, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&buf, "Message-ID: <%s@%s>\r\n", messageID, domain)
	buf.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&buf, "Content-Type: %s; charset=UTF-8\r\n", contentType)
	buf.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	qp := quotedprintable.NewWriter(&buf)
	if _, err := qp.Write([]byte(body)); err != nil {
		return "", "", nil, err
	}
	if err := qp.Close(); err != nil {
		return "", "", nil, err
	}
	return from.Address, to.Address, buf.Bytes(), nil
}

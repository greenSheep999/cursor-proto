package kernel

// Minimal IMAP-over-TLS client for pulling Cursor's magic-code emails.
//
// We only need three commands: LOGIN, SELECT INBOX, and
// SEARCH FROM "cursor.sh" SINCE <date> followed by FETCH <id> BODY[TEXT].
// go-imap is a good library but adds a dependency; the required
// surface is small enough that a hand-rolled ~250-line client using
// crypto/tls + bufio is cheaper.
//
// This is deliberately narrow: it does not implement IDLE, capability
// negotiation, or full RFC 3501. We open a fresh connection per poll
// (Cursor's magic-code arrives within seconds, and the poll retries
// every few seconds anyway).

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// imapConfig captures everything needed to poll one inbox.
type imapConfig struct {
	Host     string
	Port     int
	Username string
	Password string
}

// imapDialer is a var so tests can swap in a fake dialer that returns
// an in-memory net.Conn.
var imapDialer = defaultIMAPDial

func defaultIMAPDial(host string, port int) (net.Conn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// otpCodeRE finds a 6-digit token in the mail body. Cursor's email
// consistently formats the code as a standalone 6-digit run.
var otpCodeRE = regexp.MustCompile(`\b(\d{6})\b`)

// fetchOTPFromInbox opens a fresh IMAP session, searches for the most
// recent message from cursor.sh, and returns the 6-digit code. Returns
// ("", nil) if no matching mail exists yet — the caller should retry.
// A non-nil error is either a transport failure (retry) or an auth
// rejection (fatal).
func fetchOTPFromInbox(cfg imapConfig, sinceEpoch int64) (string, bool, error) {
	// bool return = "auth failure" (true means fatal, don't retry).
	conn, err := imapDialer(cfg.Host, cfg.Port)
	if err != nil {
		return "", false, fmt.Errorf("imap dial %s:%d: %w", cfg.Host, cfg.Port, err)
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil {
			_ = cerr
		}
	}()

	c := newIMAPConn(conn)

	// Greeting: server sends an untagged * OK line before we send anything.
	if _, err := c.readUntagged("OK"); err != nil {
		return "", false, fmt.Errorf("imap greeting: %w", err)
	}

	// LOGIN — wrap the password in quotes and escape backslashes/quotes.
	loginArgs := fmt.Sprintf(`%s %s`, imapString(cfg.Username), imapString(cfg.Password))
	if _, err := c.cmd("LOGIN", loginArgs); err != nil {
		return "", true, fmt.Errorf("imap login: %w", err)
	}

	// SELECT INBOX
	if _, err := c.cmd("SELECT", "INBOX"); err != nil {
		return "", false, fmt.Errorf("imap select: %w", err)
	}

	// SEARCH FROM "cursor.sh" SINCE dd-Mon-YYYY
	// Widen SINCE by one day to survive clock skew on the mail server.
	sinceDate := time.Unix(sinceEpoch, 0).UTC().Add(-24 * time.Hour).Format("02-Jan-2006")
	searchArgs := fmt.Sprintf(`FROM "cursor.sh" SINCE %s`, sinceDate)
	searchLines, err := c.cmd("SEARCH", searchArgs)
	if err != nil {
		return "", false, fmt.Errorf("imap search: %w", err)
	}
	ids := parseSearchResults(searchLines)
	if len(ids) == 0 {
		return "", false, nil
	}

	// Newest first — Cursor may resend and we want the freshest code.
	for i := len(ids) - 1; i >= 0; i-- {
		msgID := ids[i]
		body, err := c.fetchBody(msgID)
		if err != nil {
			// A single message failure is not fatal; try the next.
			continue
		}
		if code := extractOTPCode(body); code != "" {
			return code, false, nil
		}
	}
	return "", false, nil
}

// imapConn wraps a net.Conn with a bufio.Reader and a per-conn tag
// counter.
type imapConn struct {
	r      *bufio.Reader
	w      io.Writer
	tagNum int
}

func newIMAPConn(c net.Conn) *imapConn {
	// Give the read side a generous deadline. IMAP conversations for
	// a single poll should finish in seconds.
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	return &imapConn{r: bufio.NewReader(c), w: c}
}

// readLine reads a single CRLF-terminated line, minus the terminator.
func (c *imapConn) readLine() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	return line, nil
}

// readUntagged reads lines until an untagged OK/NO/BAD or a tagged
// completion arrives. Used only for the initial greeting.
func (c *imapConn) readUntagged(expect string) (string, error) {
	line, err := c.readLine()
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(line, "* ") {
		return "", fmt.Errorf("expected untagged, got %q", line)
	}
	rest := strings.TrimPrefix(line, "* ")
	fields := strings.Fields(rest)
	if len(fields) == 0 || fields[0] != expect {
		return line, fmt.Errorf("expected %s, got %q", expect, line)
	}
	return line, nil
}

// nextTag increments and returns the next tag id ("A0001", "A0002", ...).
func (c *imapConn) nextTag() string {
	c.tagNum++
	return fmt.Sprintf("A%04d", c.tagNum)
}

// cmd runs a single tagged command and returns the untagged response
// lines. Errors are returned when the server closes the tag with NO/BAD.
func (c *imapConn) cmd(name, args string) ([]string, error) {
	tag := c.nextTag()
	line := tag + " " + name
	if args != "" {
		line += " " + args
	}
	line += "\r\n"
	if _, err := io.WriteString(c.w, line); err != nil {
		return nil, err
	}
	var untagged []string
	for {
		resp, err := c.readLine()
		if err != nil {
			return untagged, err
		}
		if strings.HasPrefix(resp, "* ") {
			untagged = append(untagged, strings.TrimPrefix(resp, "* "))
			continue
		}
		if strings.HasPrefix(resp, "+ ") {
			// Continuation prompt — we don't do LITERALs from the
			// client side so this is unexpected.
			return untagged, fmt.Errorf("unexpected continuation: %s", resp)
		}
		if strings.HasPrefix(resp, tag+" ") {
			trailer := strings.TrimPrefix(resp, tag+" ")
			fields := strings.SplitN(trailer, " ", 2)
			switch fields[0] {
			case "OK":
				return untagged, nil
			case "NO", "BAD":
				msg := ""
				if len(fields) > 1 {
					msg = fields[1]
				}
				return untagged, fmt.Errorf("%s: %s", fields[0], msg)
			default:
				return untagged, fmt.Errorf("unknown completion %q", resp)
			}
		}
		// Some servers may interleave tagged responses from other
		// clients (shouldn't happen for us) or send unexpected lines.
		// Treat as untagged extras.
		untagged = append(untagged, resp)
	}
}

// fetchBody runs FETCH <id> BODY.PEEK[] and returns the message body.
// BODY.PEEK[] returns headers + body without marking the message read.
func (c *imapConn) fetchBody(msgID string) (string, error) {
	tag := c.nextTag()
	if _, err := io.WriteString(c.w, tag+" FETCH "+msgID+" BODY.PEEK[]\r\n"); err != nil {
		return "", err
	}
	var body strings.Builder
	for {
		line, err := c.readLine()
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(line, tag+" ") {
			trailer := strings.TrimPrefix(line, tag+" ")
			fields := strings.SplitN(trailer, " ", 2)
			if fields[0] == "OK" {
				return body.String(), nil
			}
			return "", fmt.Errorf("fetch %s: %s", msgID, trailer)
		}
		if strings.HasPrefix(line, "* ") {
			// Header line like: * 1 FETCH (BODY[] {1234}
			// The braced number is the literal byte count that follows.
			if idx := strings.LastIndex(line, "{"); idx >= 0 && strings.HasSuffix(line, "}") {
				numStr := line[idx+1 : len(line)-1]
				n, err := strconv.Atoi(numStr)
				if err == nil && n > 0 {
					buf := make([]byte, n)
					if _, err := io.ReadFull(c.r, buf); err != nil {
						return "", err
					}
					body.Write(buf)
					// The line following the literal is either a ")"
					// closing the FETCH parens (possibly with more
					// data flags) or an untagged line. Consume until
					// we see the tagged completion.
					continue
				}
			}
			// Non-literal fetch response (e.g. some servers use quoted
			// strings). Best-effort: skip it.
			continue
		}
	}
}

// parseSearchResults extracts message ids from IMAP SEARCH lines.
// Example untagged line: `SEARCH 1 2 3`.
func parseSearchResults(lines []string) []string {
	var ids []string
	for _, line := range lines {
		if !strings.HasPrefix(strings.ToUpper(line), "SEARCH") {
			continue
		}
		rest := strings.TrimSpace(line[len("SEARCH"):])
		if rest == "" {
			continue
		}
		for _, tok := range strings.Fields(rest) {
			if _, err := strconv.Atoi(tok); err == nil {
				ids = append(ids, tok)
			}
		}
	}
	return ids
}

// imapString quotes an IMAP astring — wraps in double quotes and
// backslash-escapes " and \.
func imapString(s string) string {
	esc := strings.ReplaceAll(s, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	return `"` + esc + `"`
}

// extractOTPCode pulls the first 6-digit run out of a raw MIME body.
// Attempts base64 and quoted-printable decoding on the body sections
// before matching, since Cursor's emails are typically MIME multipart
// with base64-encoded text/plain and text/html parts.
func extractOTPCode(raw string) string {
	// First try the raw body verbatim.
	if m := otpCodeRE.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	// Decode any base64 blocks and search again.
	for _, block := range extractBase64Blocks(raw) {
		if decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(block), "")); err == nil {
			if m := otpCodeRE.FindStringSubmatch(string(decoded)); m != nil {
				return m[1]
			}
		}
	}
	// Try quoted-printable decoding — some providers use it.
	if decoded, err := quotedPrintableDecode(raw); err == nil {
		if m := otpCodeRE.FindStringSubmatch(decoded); m != nil {
			return m[1]
		}
	}
	return ""
}

// extractBase64Blocks pulls runs of base64-alphabet characters out of
// a MIME body. Very approximate — we look for long enough contiguous
// blocks separated by blank lines.
func extractBase64Blocks(raw string) []string {
	var blocks []string
	sections := strings.Split(raw, "\r\n\r\n")
	for _, sec := range sections {
		// A base64 section has only base64 chars + whitespace.
		trimmed := strings.TrimSpace(sec)
		if len(trimmed) < 16 {
			continue
		}
		if isMostlyBase64(trimmed) {
			blocks = append(blocks, trimmed)
		}
	}
	// Fall back: also split by "\n\n" for LF-only bodies.
	if len(blocks) == 0 {
		for _, sec := range strings.Split(raw, "\n\n") {
			trimmed := strings.TrimSpace(sec)
			if len(trimmed) >= 16 && isMostlyBase64(trimmed) {
				blocks = append(blocks, trimmed)
			}
		}
	}
	return blocks
}

// isMostlyBase64 reports whether a run consists (mostly) of base64
// characters. We tolerate stray whitespace and skip headers.
func isMostlyBase64(s string) bool {
	good := 0
	bad := 0
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			good++
		case r == '+', r == '/', r == '=':
			good++
		case r == '\r', r == '\n', r == ' ', r == '\t':
			// ignore
		default:
			bad++
		}
	}
	return good > 16 && bad == 0
}

// quotedPrintableDecode is a tiny quoted-printable decoder for MIME
// bodies (RFC 2045 §6.7). Handles =XX hex escapes and soft line
// breaks (=\r\n or =\n).
func quotedPrintableDecode(s string) (string, error) {
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] != '=' {
			out.WriteByte(s[i])
			i++
			continue
		}
		// s[i] == '='
		if i+1 < len(s) && (s[i+1] == '\r' || s[i+1] == '\n') {
			// soft line break
			i++
			if i < len(s) && s[i] == '\r' {
				i++
			}
			if i < len(s) && s[i] == '\n' {
				i++
			}
			continue
		}
		if i+2 >= len(s) {
			return "", errors.New("invalid quoted-printable: dangling =")
		}
		hex := s[i+1 : i+3]
		v, err := strconv.ParseUint(hex, 16, 8)
		if err != nil {
			// Not valid hex — copy through verbatim and continue.
			out.WriteByte(s[i])
			i++
			continue
		}
		out.WriteByte(byte(v))
		i += 3
	}
	return out.String(), nil
}

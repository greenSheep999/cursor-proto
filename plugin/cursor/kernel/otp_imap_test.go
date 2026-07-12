package kernel

// Unit tests for the tiny IMAP client used by the Cursor OTP flow.
//
// The end-to-end connection path is exercised through a
// net.Pipe-backed fake IMAP server; the body-decoding helpers are
// exercised directly with representative Cursor email bodies.

import (
	"bufio"
	"encoding/base64"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestExtractOTPCode covers the three text shapes we've seen from
// Cursor's magic-code emails: plain text, base64-encoded MIME parts,
// and quoted-printable HTML.
func TestExtractOTPCode(t *testing.T) {
	cases := map[string]string{
		"Your Cursor sign-in code is 928374. Do not share it.": "928374",
		// base64 block
		"Content-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte("Your code is 111222")): "111222",
		"no code here at all": "",
	}
	for body, want := range cases {
		got := extractOTPCode(body)
		if got != want {
			t.Errorf("extractOTPCode(%q) = %q, want %q", body, got, want)
		}
	}
	// Quoted-printable — a soft break in the middle of the digits
	// should NOT split the match after decoding.
	qp := "Content-Transfer-Encoding: quoted-printable\r\n\r\nCode: 4=\r\n56789 is your login"
	if got := extractOTPCode(qp); got != "456789" {
		t.Errorf("qp decode failed: %q", got)
	}
}

// TestParseSearchResults exercises the SEARCH response parser.
func TestParseSearchResults(t *testing.T) {
	if got := parseSearchResults([]string{"SEARCH 1 5 12", "OK done"}); len(got) != 3 || got[0] != "1" || got[2] != "12" {
		t.Errorf("parse failed: %v", got)
	}
	if got := parseSearchResults([]string{"SEARCH"}); len(got) != 0 {
		t.Errorf("empty SEARCH should return 0 ids: %v", got)
	}
	if got := parseSearchResults([]string{"OK"}); len(got) != 0 {
		t.Errorf("non-SEARCH lines should be skipped: %v", got)
	}
}

// TestIMAPClient_HappyPath drives fetchOTPFromInbox against an
// in-memory fake IMAP server so we cover the wire protocol without a
// real TLS connection.
func TestIMAPClient_HappyPath(t *testing.T) {
	clientEnd, serverEnd := net.Pipe()

	// Swap the dialer to hand back our pipe end.
	orig := imapDialer
	t.Cleanup(func() { imapDialer = orig })
	imapDialer = func(host string, port int) (net.Conn, error) {
		return clientEnd, nil
	}

	// Fake IMAP server: run in a goroutine and script the expected
	// conversation.
	done := make(chan error, 1)
	go func() {
		defer func() {
			if cerr := serverEnd.Close(); cerr != nil {
				_ = cerr
			}
		}()
		br := bufio.NewReader(serverEnd)
		// Greeting.
		_, err := io.WriteString(serverEnd, "* OK IMAP4rev1 fake server ready\r\n")
		if err != nil {
			done <- err
			return
		}
		// LOGIN
		line, err := br.ReadString('\n')
		if err != nil {
			done <- err
			return
		}
		if !strings.Contains(line, "LOGIN") {
			done <- io.ErrClosedPipe
			return
		}
		tag := strings.SplitN(line, " ", 2)[0]
		if _, err := io.WriteString(serverEnd, tag+" OK login ok\r\n"); err != nil {
			done <- err
			return
		}
		// SELECT
		line, err = br.ReadString('\n')
		if err != nil {
			done <- err
			return
		}
		tag = strings.SplitN(line, " ", 2)[0]
		if _, err := io.WriteString(serverEnd, "* 3 EXISTS\r\n"+tag+" OK [READ-WRITE] SELECT completed\r\n"); err != nil {
			done <- err
			return
		}
		// SEARCH
		line, err = br.ReadString('\n')
		if err != nil {
			done <- err
			return
		}
		tag = strings.SplitN(line, " ", 2)[0]
		if _, err := io.WriteString(serverEnd, "* SEARCH 3\r\n"+tag+" OK SEARCH completed\r\n"); err != nil {
			done <- err
			return
		}
		// FETCH — deliver a body containing the OTP.
		line, err = br.ReadString('\n')
		if err != nil {
			done <- err
			return
		}
		tag = strings.SplitN(line, " ", 2)[0]
		body := "From: Cursor <noreply@cursor.sh>\r\nSubject: Your sign-in code\r\n\r\nYour code is 445566\r\n"
		fetchResp := "* 3 FETCH (BODY[] {" + itoa(len(body)) + "}\r\n" + body + ")\r\n" + tag + " OK FETCH completed\r\n"
		if _, err := io.WriteString(serverEnd, fetchResp); err != nil {
			done <- err
			return
		}
		done <- nil
	}()

	cfg := imapConfig{Host: "imap.example.com", Port: 993, Username: "u", Password: "p"}
	code, fatal, err := fetchOTPFromInbox(cfg, time.Now().Unix())
	if err != nil {
		t.Fatalf("fetchOTPFromInbox: %v (fatal=%v)", err, fatal)
	}
	if code != "445566" {
		t.Errorf("code = %q, want 445566", code)
	}
	if fatal {
		t.Errorf("fatal should be false on happy path")
	}
	if err := <-done; err != nil && err != io.ErrClosedPipe {
		t.Logf("server goroutine finished with: %v", err)
	}
}

// TestIMAPClient_AuthFailure verifies that a NO response to LOGIN is
// surfaced as a fatal error, not a transient one.
func TestIMAPClient_AuthFailure(t *testing.T) {
	clientEnd, serverEnd := net.Pipe()
	orig := imapDialer
	t.Cleanup(func() { imapDialer = orig })
	imapDialer = func(host string, port int) (net.Conn, error) { return clientEnd, nil }

	go func() {
		defer func() { _ = serverEnd.Close() }()
		br := bufio.NewReader(serverEnd)
		_, _ = io.WriteString(serverEnd, "* OK ready\r\n")
		line, _ := br.ReadString('\n')
		tag := strings.SplitN(line, " ", 2)[0]
		_, _ = io.WriteString(serverEnd, tag+" NO invalid credentials\r\n")
	}()

	_, fatal, err := fetchOTPFromInbox(imapConfig{Host: "x", Port: 993, Username: "u", Password: "p"}, 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if !fatal {
		t.Errorf("expected fatal=true for auth failure, got fatal=false err=%v", err)
	}
}

// itoa is a tiny stdlib-free stringifier so the test goroutine avoids
// a strconv import.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [16]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

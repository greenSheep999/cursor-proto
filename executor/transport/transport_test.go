package transport

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParse_Aliases(t *testing.T) {
	cases := []struct {
		in   string
		want Version
	}{
		{"", Auto},
		{"auto", Auto},
		{"AUTO", Auto},
		{"h2", Auto},
		{"http/2", Auto},
		{"http1.1", Http1_1},
		{"HTTP1.1", Http1_1},
		{"h1", Http1_1},
		{"http/1.1", Http1_1},
		{"1.1", Http1_1},
		{"http1.0", Http1_0},
		{"1.0", Http1_0},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): err=%v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParse_RejectsGarbage(t *testing.T) {
	for _, bad := range []string{"h3", "http3", "quic", "http/9000", "yes please"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) = nil err; want error", bad)
		}
	}
}

func TestVersion_String_Roundtrip(t *testing.T) {
	for _, v := range []Version{Auto, Http1_1, Http1_0} {
		s := v.String()
		back, err := Parse(s)
		if err != nil {
			t.Errorf("Parse(%q) after String: %v", s, err)
			continue
		}
		if back != v {
			t.Errorf("roundtrip %v→%q→%v mismatch", v, s, back)
		}
	}
}

func TestNew_Http1_1_DisablesH2(t *testing.T) {
	tr := New(Http1_1)
	if tr.ForceAttemptHTTP2 {
		t.Error("Http1_1 Transport still has ForceAttemptHTTP2=true")
	}
	// TLSNextProto is the ALPN callback map. Assigning an empty map
	// wipes any registered protocol (including h2 that Go's h2 bundle
	// registers via init). We don't check *what* was in there before —
	// just that we successfully cleared it.
	if len(tr.TLSNextProto) != 0 {
		t.Errorf("Http1_1 Transport TLSNextProto not empty: %d entries", len(tr.TLSNextProto))
	}
}

func TestNew_Http1_0_DisablesKeepalive(t *testing.T) {
	tr := New(Http1_0)
	if tr.ForceAttemptHTTP2 {
		t.Error("Http1_0 Transport still has ForceAttemptHTTP2=true")
	}
	if !tr.DisableKeepAlives {
		t.Error("Http1_0 Transport still has keepalives enabled")
	}
	if len(tr.TLSNextProto) != 0 {
		t.Errorf("Http1_0 Transport TLSNextProto not empty: %d entries", len(tr.TLSNextProto))
	}
}

func TestNew_Auto_LeavesDefaults(t *testing.T) {
	tr := New(Auto)
	// The default Transport has ForceAttemptHTTP2=true since Go 1.11ish.
	if !tr.ForceAttemptHTTP2 {
		t.Error("Auto Transport should keep ForceAttemptHTTP2=true")
	}
	// We don't assert on TLSNextProto contents — Go populates it
	// lazily via sync.Once from h2 bundle init, so a cloned Transport
	// might legitimately have an empty map until first use. What
	// matters is that we didn't overwrite it.
	if tr.DisableKeepAlives {
		t.Error("Auto Transport should not disable keepalives")
	}
}

// stubRoundTripper captures the request it received so we can assert on
// what wrapper layers did to it before it reached the base.
type stubRoundTripper struct{ seen *http.Request }

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.seen = req
	return &http.Response{StatusCode: 200, Body: http.NoBody, Request: req}, nil
}

func TestHTTP1_0RoundTripper_RewritesRequestProto(t *testing.T) {
	// We test the wrapper's direct behavior — did it set the request's
	// Proto fields to HTTP/1.0 before handing off to the base? We do
	// NOT test the wire, because Go's built-in http.Transport writes
	// HTTP/1.1 on the wire regardless of req.Proto (see the Http1_0
	// docstring). The wrapper's job is to make Request.Proto reflect
	// the operator's intent for application-level middleware and logs.
	base := &stubRoundTripper{}
	wrapper := HTTP1_0RoundTripper{Base: base}

	req, err := http.NewRequest("GET", "http://example.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a caller that set Proto to HTTP/1.1 (the Go default).
	req.Proto = "HTTP/1.1"
	req.ProtoMajor = 1
	req.ProtoMinor = 1

	if _, err := wrapper.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if base.seen == nil {
		t.Fatal("base RT never called")
	}
	if base.seen.Proto != "HTTP/1.0" {
		t.Errorf("base saw Proto=%q, want HTTP/1.0", base.seen.Proto)
	}
	if base.seen.ProtoMajor != 1 || base.seen.ProtoMinor != 0 {
		t.Errorf("base saw ProtoMajor/Minor=%d/%d, want 1/0",
			base.seen.ProtoMajor, base.seen.ProtoMinor)
	}
	// The original request the caller handed us must be unchanged —
	// RoundTrip contract requires this.
	if req.Proto != "HTTP/1.1" {
		t.Errorf("wrapper mutated caller's req.Proto to %q", req.Proto)
	}
}

func TestHTTP1_0RoundTripper_ForwardsHeaders(t *testing.T) {
	// Sanity: the wrapper shouldn't drop headers when it clones.
	base := &stubRoundTripper{}
	wrapper := HTTP1_0RoundTripper{Base: base}
	req, _ := http.NewRequest("POST", "http://example.invalid/", nil)
	req.Header.Set("X-Test", "hello")
	req.Header.Set("Authorization", "Bearer token")
	if _, err := wrapper.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if base.seen.Header.Get("X-Test") != "hello" {
		t.Error("X-Test header lost through wrapper")
	}
	if base.seen.Header.Get("Authorization") != "Bearer token" {
		t.Error("Authorization header lost through wrapper")
	}
}

func TestClient_Builds(t *testing.T) {
	for _, v := range []Version{Auto, Http1_1, Http1_0} {
		c := Client(v, 5*time.Second)
		if c.Timeout != 5*time.Second {
			t.Errorf("%v: timeout wrong", v)
		}
		if c.Transport == nil {
			t.Errorf("%v: nil transport", v)
		}
		_, wrapped := c.Transport.(HTTP1_0RoundTripper)
		if v == Http1_0 && !wrapped {
			t.Errorf("Http1_0 client should have HTTP1_0RoundTripper")
		}
		if v != Http1_0 && wrapped {
			t.Errorf("%v client should not have HTTP1_0RoundTripper", v)
		}
	}
}

// Integration: fire a real request through Client(Http1_1) at a
// httptest.Server. The server records the request's Proto — since
// httptest.NewServer speaks plain HTTP/1.x (no TLS, no ALPN), this
// exercises the plumbing without requiring a live upstream.
func TestClient_Http1_1_HitsLocalServer(t *testing.T) {
	var seenProto string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenProto = r.Proto
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := Client(Http1_1, 5*time.Second)
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seenProto != "HTTP/1.1" {
		t.Errorf("server saw proto %q, want HTTP/1.1", seenProto)
	}
}

// Belt-and-braces: assert Transport.Clone() gave us a fresh instance,
// not a shared pointer that would leak the disable-h2 mutation into
// http.DefaultTransport (which would break other code paths in the
// process — nasty regression).
func TestNew_DoesNotMutateDefault(t *testing.T) {
	before := http.DefaultTransport.(*http.Transport)
	beforeForceH2 := before.ForceAttemptHTTP2
	beforeNextProto := len(before.TLSNextProto)

	_ = New(Http1_1)
	_ = New(Http1_0)

	after := http.DefaultTransport.(*http.Transport)
	if after.ForceAttemptHTTP2 != beforeForceH2 {
		t.Errorf("New() mutated http.DefaultTransport.ForceAttemptHTTP2")
	}
	if len(after.TLSNextProto) != beforeNextProto {
		t.Errorf("New() mutated http.DefaultTransport.TLSNextProto")
	}
}

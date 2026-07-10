package executor

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// TrailerStatus captures the gRPC-web trailer that terminates a Connect stream.
// A non-zero Code means the upstream refused (or aborted) the call; the proxy
// must surface that to the API caller instead of silently returning empty
// content.
type TrailerStatus struct {
	// Code mirrors google.rpc.Code (0=OK, 8=RESOURCE_EXHAUSTED / model-gate, etc.).
	Code int
	// Message is the raw grpc-message header value (URL-unescaped).
	Message string
	// DetailsB64 is the untouched grpc-status-details-bin header value.
	DetailsB64 string
	// Detail is the decoded aiserver.v1.ErrorDetails, when present.
	Detail *cursorpb.AiserverV1_ErrorDetails
}

// OK reports whether this trailer represents a successful termination.
func (t *TrailerStatus) OK() bool {
	return t == nil || t.Code == 0
}

// Err returns an error describing the trailer, or nil when OK.
func (t *TrailerStatus) Err() error {
	if t.OK() {
		return nil
	}
	msg := describeErrorDetail(t.Detail)
	if msg == "" {
		msg = t.Message
	}
	if msg == "" {
		msg = "upstream error"
	}
	code := ""
	if t.Detail != nil {
		code = t.Detail.GetError().String()
	}
	if code != "" {
		return fmt.Errorf("cursor upstream: %s (grpc-status=%d, %s)", msg, t.Code, code)
	}
	return fmt.Errorf("cursor upstream: %s (grpc-status=%d)", msg, t.Code)
}

// ParseTrailer turns the raw trailer payload (HTTP-header-style text) into a
// TrailerStatus. Best-effort: unknown keys are ignored, and unparseable
// grpc-status-details-bin is left in DetailsB64 without a decoded Detail.
func ParseTrailer(raw []byte) *TrailerStatus {
	if len(raw) == 0 {
		return nil
	}
	t := &TrailerStatus{}
	text := string(raw)
	// grpc-web trailer separator is CRLF, but some servers omit CR. Accept both.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:i]))
		val := strings.TrimSpace(line[i+1:])
		switch key {
		case "grpc-status":
			if n, err := strconv.Atoi(val); err == nil {
				t.Code = n
			}
		case "grpc-message":
			t.Message = val
		case "grpc-status-details-bin":
			t.DetailsB64 = val
			// Base64 decoding is standard base64 without padding in gRPC-web.
			t.Detail = decodeStatusDetails(val)
		}
	}
	return t
}

// decodeStatusDetails parses one gRPC status-details-bin blob. The wire format
// is a google.rpc.Status message; the interior detail we care about is the
// packed aiserver.v1.ErrorDetails, which Cursor wraps in an Any.
func decodeStatusDetails(b64 string) *cursorpb.AiserverV1_ErrorDetails {
	if b64 == "" {
		return nil
	}
	// gRPC-web uses standard base64, but some proxies re-encode without
	// padding. Try both.
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(b64)
		if err != nil {
			return nil
		}
	}
	// The payload is a google.rpc.Status: field 1 = code (varint), field 2 =
	// message (string), field 3 = details (repeated Any). Scan for the first
	// Any whose type_url is aiserver.v1.ErrorDetails and try to unmarshal its
	// value into our generated proto. Any is
	// { 1: string type_url, 2: bytes value }.
	details := scanForCursorErrorDetails(raw)
	if len(details) == 0 {
		return nil
	}
	out := &cursorpb.AiserverV1_ErrorDetails{}
	if err := proto.Unmarshal(details, out); err != nil {
		return nil
	}
	return out
}

// scanForCursorErrorDetails walks a google.rpc.Status blob and returns the
// value bytes of the first google.protobuf.Any whose type_url mentions
// "aiserver.v1.ErrorDetails". Robust to unknown fields.
func scanForCursorErrorDetails(buf []byte) []byte {
	for len(buf) > 0 {
		tag, n := readVarint(buf)
		if n <= 0 {
			return nil
		}
		buf = buf[n:]
		field := tag >> 3
		wire := tag & 0x07
		switch wire {
		case 0: // varint
			_, m := readVarint(buf)
			if m <= 0 {
				return nil
			}
			buf = buf[m:]
		case 2: // length-delimited
			l, m := readVarint(buf)
			if m <= 0 {
				return nil
			}
			buf = buf[m:]
			if uint64(len(buf)) < l {
				return nil
			}
			body := buf[:l]
			buf = buf[l:]
			if field == 3 {
				if v := unpackAny(body); len(v) > 0 {
					return v
				}
			}
		default:
			return nil
		}
	}
	return nil
}

// unpackAny decodes a google.protobuf.Any and returns its value bytes when the
// type_url identifies aiserver.v1.ErrorDetails, otherwise nil.
func unpackAny(buf []byte) []byte {
	var typeURL string
	var value []byte
	for len(buf) > 0 {
		tag, n := readVarint(buf)
		if n <= 0 {
			return nil
		}
		buf = buf[n:]
		field := tag >> 3
		wire := tag & 0x07
		if wire != 2 {
			return nil
		}
		l, m := readVarint(buf)
		if m <= 0 || uint64(len(buf)-m) < l {
			return nil
		}
		buf = buf[m:]
		body := buf[:l]
		buf = buf[l:]
		switch field {
		case 1:
			typeURL = string(body)
		case 2:
			value = body
		}
	}
	if strings.Contains(typeURL, "aiserver.v1.ErrorDetails") {
		return value
	}
	return nil
}

// readVarint decodes a protobuf varint from the head of buf. Returns the
// decoded value and the number of bytes consumed, or (0, 0) on error.
func readVarint(buf []byte) (uint64, int) {
	var v uint64
	var shift uint
	for i, b := range buf {
		v |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return v, i + 1
		}
		shift += 7
		if shift >= 64 {
			return 0, 0
		}
	}
	return 0, 0
}

// describeErrorDetail extracts a human-readable message from an
// aiserver.v1.ErrorDetails.Details, favoring the innermost user-visible copy.
func describeErrorDetail(d *cursorpb.AiserverV1_ErrorDetails) string {
	if d == nil {
		return ""
	}
	inner := d.GetDetails()
	if inner == nil {
		return ""
	}
	title := inner.GetTitle()
	detail := inner.GetDetail()
	switch {
	case title != "" && detail != "":
		return title + ": " + detail
	case detail != "":
		return detail
	case title != "":
		return title
	}
	return ""
}

// ErrTrailerFailed is returned by helper wrappers when the stream terminated
// with a non-zero grpc-status.
var ErrTrailerFailed = errors.New("upstream trailer reported failure")

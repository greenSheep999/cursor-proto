package executor

import (
	"encoding/binary"
	"testing"
)

func TestParseSandBoxBodyReadsRunState(t *testing.T) {
	// field 10 string url, field 11 string token, field 13 varint 3
	var payload []byte
	payload = appendProtoString(payload, 10, "https://box.example")
	payload = appendProtoString(payload, 11, "box-token")
	payload = appendProtoVarint(payload, 13, sandBoxRunStateRunning)

	frame := make([]byte, 5+len(payload))
	frame[0] = 0
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)

	url, token, _, state, err := parseSandBoxBody(frame)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://box.example" || token != "box-token" {
		t.Fatalf("url=%q token=%q", url, token)
	}
	if state != sandBoxRunStateRunning {
		t.Fatalf("runState=%d want 3", state)
	}
}

func appendProtoString(buf []byte, field int, value string) []byte {
	buf = appendTestVarint(buf, uint64(field<<3|2))
	buf = appendTestVarint(buf, uint64(len(value)))
	return append(buf, value...)
}

func appendProtoVarint(buf []byte, field int, value int) []byte {
	buf = appendTestVarint(buf, uint64(field<<3))
	return appendTestVarint(buf, uint64(value))
}

func appendTestVarint(buf []byte, value uint64) []byte {
	for value >= 0x80 {
		buf = append(buf, byte(value)|0x80)
		value >>= 7
	}
	return append(buf, byte(value))
}

package logs

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLogRecord_Encode(t *testing.T) {
	r := NewRecord(SourceSelf, LevelWarning, "reconnecting in 2.053s")
	r.Component = "grpc"

	line, err := r.Encode()
	if err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("encoded line is not valid JSON: %v", err)
	}
	if got["level"] != "warn" {
		t.Errorf("level = %v, want warn", got["level"])
	}
	if got["source"] != SourceSelf {
		t.Errorf("source = %v, want self", got["source"])
	}
	if got["component"] != "grpc" {
		t.Errorf("component = %v, want grpc", got["component"])
	}
	ts, _ := got["ts"].(string)
	if !strings.HasSuffix(ts, "Z") || len(ts) != 24 || ts[19] != '.' {
		t.Errorf("ts = %q, want RFC3339 with milliseconds in UTC", ts)
	}
	if _, ok := got["fields"]; ok {
		t.Errorf("empty fields must be omitted")
	}
}

func TestLogRecord_EncodeRedactsMessageAndFields(t *testing.T) {
	r := NewRecord(SourceApp, LevelInfo, "connected with sk-abc123def456ghi789jkl")
	r.Fields = map[string]string{"password": "hunter2", "user": "amy"}

	line, err := r.Encode()
	if err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	encoded := string(line)
	if strings.Contains(encoded, "sk-abc123def456ghi789jkl") {
		t.Errorf("api key leaked into encoded record: %s", encoded)
	}
	if strings.Contains(encoded, "hunter2") {
		t.Errorf("field value leaked into encoded record: %s", encoded)
	}
	if !strings.Contains(encoded, `"user":"amy"`) {
		t.Errorf("benign field value must survive redaction: %s", encoded)
	}
}

func TestLogRecord_CapsMessage(t *testing.T) {
	r := NewRecord(SourceApp, LevelInfo, strings.Repeat("x", maxRecordMsg+64))
	if len(r.Msg) != maxRecordMsg+64 {
		t.Fatalf("NewRecord must not modify the raw message")
	}
	line, err := r.Encode()
	if err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	var got LogRecord
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("encoded line is not valid JSON: %v", err)
	}
	if len(got.Msg) != maxRecordMsg {
		t.Errorf("encoded message length = %d, want %d", len(got.Msg), maxRecordMsg)
	}
}

func TestLogRecord_CapsMessageOnRuneBoundary(t *testing.T) {
	r := NewRecord(SourceApp, LevelInfo, strings.Repeat("é", maxRecordMsg/2+32))
	line, err := r.Encode()
	if err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	var got LogRecord
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("encoded line is not valid JSON: %v", err)
	}
	if strings.Contains(got.Msg, "\uFFFD") {
		t.Errorf("message truncated mid-rune: %q", got.Msg[len(got.Msg)-8:])
	}
}

func TestParseRecordTime(t *testing.T) {
	if !ParseRecordTime("").IsZero() {
		t.Errorf("empty ts must parse to the zero time")
	}
	if !ParseRecordTime("not-a-time").IsZero() {
		t.Errorf("garbage ts must parse to the zero time")
	}
	want := "2026-10-29T18:04:05.123Z"
	got := ParseRecordTime(want)
	if got.UTC().Format(RFC3339Milli) != want {
		t.Errorf("ParseRecordTime(%q) = %v", want, got)
	}
	plain := ParseRecordTime("2026-10-29T18:04:05Z")
	if plain.IsZero() {
		t.Errorf("RFC3339 without millis must still parse")
	}
}

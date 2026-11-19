package api

import (
	"encoding/json"
	"testing"
	"time"
)

// Compile-time checks that Time implements the JSON interfaces.
var (
	_ json.Marshaler   = Time{}
	_ json.Unmarshaler = (*Time)(nil)
)

func TestTimeMarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			name: "UTC with zero milliseconds",
			in:   time.Date(2026, 11, 19, 6, 30, 0, 0, time.UTC),
			want: `"2026-11-19T06:30:00.000Z"`,
		},
		{
			name: "converts an offset to UTC",
			in:   time.Date(2026, 11, 19, 14, 30, 0, 123456789, time.FixedZone("CST", 8*60*60)),
			want: `"2026-11-19T06:30:00.123Z"`,
		},
		{
			name: "truncates sub-millisecond digits",
			in:   time.Date(2026, 11, 19, 6, 30, 0, 999999999, time.UTC),
			want: `"2026-11-19T06:30:00.999Z"`,
		},
		{
			name: "pads the milliseconds to three digits",
			in:   time.Date(2026, 11, 19, 6, 30, 0, 5_000_000, time.UTC),
			want: `"2026-11-19T06:30:00.005Z"`,
		},
		{
			name: "converts a negative offset to UTC",
			in:   time.Date(2026, 11, 18, 23, 30, 0, 0, time.FixedZone("MST", -7*60*60)),
			want: `"2026-11-19T06:30:00.000Z"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(Time{tt.in})
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("json.Marshal() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestTimeUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Time
	}{
		{
			name: "UTC with milliseconds",
			in:   `"2026-11-19T06:30:00.123Z"`,
			want: time.Date(2026, 11, 19, 6, 30, 0, 123_000_000, time.UTC),
		},
		{
			name: "positive offset with nanoseconds",
			in:   `"2026-11-19T14:30:00.123456789+08:00"`,
			want: time.Date(2026, 11, 19, 6, 30, 0, 123456789, time.UTC),
		},
		{
			name: "negative offset",
			in:   `"2026-11-18T22:30:00.123-08:00"`,
			want: time.Date(2026, 11, 19, 6, 30, 0, 123_000_000, time.UTC),
		},
		{
			name: "no fractional digits",
			in:   `"2026-11-19T06:30:00Z"`,
			want: time.Date(2026, 11, 19, 6, 30, 0, 0, time.UTC),
		},
		{
			name: "one fractional digit",
			in:   `"2026-11-19T06:30:00.5Z"`,
			want: time.Date(2026, 11, 19, 6, 30, 0, 500_000_000, time.UTC),
		},
		{
			name: "six fractional digits",
			in:   `"2026-11-19T06:30:00.999999Z"`,
			want: time.Date(2026, 11, 19, 6, 30, 0, 999_999_000, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Time
			if err := json.Unmarshal([]byte(tt.in), &got); err != nil {
				t.Fatalf("json.Unmarshal(%s) error = %v", tt.in, err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("json.Unmarshal(%s) = %v, want %v", tt.in, got.Time, tt.want)
			}
		})
	}
}

func TestTimeUnmarshalJSONInvalid(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "date only", in: `"2026-11-19"`},
		{name: "missing zone", in: `"2026-11-19T06:30:00"`},
		{name: "missing zone with fraction", in: `"2026-11-19T06:30:00.123"`},
		{name: "out of range month", in: `"2026-13-01T00:00:00Z"`},
		{name: "empty string", in: `""`},
		{name: "free text", in: `"not a timestamp"`},
		{name: "not a string", in: `123`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Time
			if err := json.Unmarshal([]byte(tt.in), &got); err == nil {
				t.Errorf("json.Unmarshal(%s) error = nil, want a parse error", tt.in)
			}
		})
	}
}

func TestOptionalFieldsEncodeAsNull(t *testing.T) {
	start := time.Date(2026, 11, 19, 6, 30, 0, 0, time.UTC)

	t.Run("Turn with an unfinished turn", func(t *testing.T) {
		turn := Turn{TurnID: "turn-1", StartedAt: Time{start}}
		want := `{"turn_id":"turn-1","started_at":"2026-11-19T06:30:00.000Z","ended_at":null,"output":"","is_error":false}`

		got, err := json.Marshal(turn)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		if string(got) != want {
			t.Errorf("json.Marshal() = %s, want %s", got, want)
		}
	})

	t.Run("Turn with a finished turn", func(t *testing.T) {
		end := time.Date(2026, 11, 19, 6, 30, 5, 250_000_000, time.UTC)
		turn := Turn{TurnID: "turn-1", StartedAt: Time{start}, EndedAt: &Time{end}, Output: "done", IsError: true}
		want := `{"turn_id":"turn-1","started_at":"2026-11-19T06:30:00.000Z","ended_at":"2026-11-19T06:30:05.250Z","output":"done","is_error":true}`

		got, err := json.Marshal(turn)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		if string(got) != want {
			t.Errorf("json.Marshal() = %s, want %s", got, want)
		}
	})

	t.Run("Suspension with unfinished timings", func(t *testing.T) {
		suspension := Suspension{StartedAt: Time{time.Date(2026, 11, 19, 6, 31, 0, 0, time.UTC)}}
		want := `{"started_at":"2026-11-19T06:31:00.000Z","snapshot_ms":null,"restore_ms":null,"hello_ms":null,"error":""}`

		got, err := json.Marshal(suspension)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		if string(got) != want {
			t.Errorf("json.Marshal() = %s, want %s", got, want)
		}
	})
}

func TestOptionalFieldsDecodeFromNullAndValues(t *testing.T) {
	t.Run("Turn", func(t *testing.T) {
		var nullTurn Turn
		if err := json.Unmarshal([]byte(`{"turn_id":"turn-1","started_at":"2026-11-19T06:30:00.000Z","ended_at":null,"output":"","is_error":false}`), &nullTurn); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		if nullTurn.EndedAt != nil {
			t.Errorf("EndedAt = %v, want nil", nullTurn.EndedAt)
		}

		var finished Turn
		if err := json.Unmarshal([]byte(`{"turn_id":"turn-1","started_at":"2026-11-19T06:30:00.000Z","ended_at":"2026-11-19T06:30:05.250Z","output":"done","is_error":true}`), &finished); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		want := time.Date(2026, 11, 19, 6, 30, 5, 250_000_000, time.UTC)
		if finished.EndedAt == nil || !finished.EndedAt.Equal(want) {
			t.Errorf("EndedAt = %v, want %v", finished.EndedAt, want)
		}
	})

	t.Run("Suspension", func(t *testing.T) {
		var unfinished Suspension
		if err := json.Unmarshal([]byte(`{"started_at":"2026-11-19T06:31:00.000Z","snapshot_ms":null,"restore_ms":null,"hello_ms":null,"error":""}`), &unfinished); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		if unfinished.SnapshotMS != nil || unfinished.RestoreMS != nil || unfinished.HelloMS != nil {
			t.Errorf("timings = (%v, %v, %v), want all nil",
				unfinished.SnapshotMS, unfinished.RestoreMS, unfinished.HelloMS)
		}

		var timed Suspension
		if err := json.Unmarshal([]byte(`{"started_at":"2026-11-19T06:31:00.000Z","snapshot_ms":12,"restore_ms":34,"hello_ms":56,"error":"snapshot failed"}`), &timed); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		if timed.SnapshotMS == nil || *timed.SnapshotMS != 12 {
			t.Errorf("SnapshotMS = %v, want 12", timed.SnapshotMS)
		}
		if timed.RestoreMS == nil || *timed.RestoreMS != 34 {
			t.Errorf("RestoreMS = %v, want 34", timed.RestoreMS)
		}
		if timed.HelloMS == nil || *timed.HelloMS != 56 {
			t.Errorf("HelloMS = %v, want 56", timed.HelloMS)
		}
		if timed.Error != "snapshot failed" {
			t.Errorf("Error = %q, want %q", timed.Error, "snapshot failed")
		}
	})
}

// TestRoundTrip decodes hand-written JSON and re-encodes it. The input is
// written out literally, not generated from these types, so the field names
// and their order are checked against the milestone document.
func TestRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		json string
		into any
	}{
		{
			name: "CreateRunRequest",
			json: `{"prompt":"hello"}`,
			into: new(CreateRunRequest),
		},
		{
			name: "CreateRunResponse",
			json: `{"run_id":"run-1"}`,
			into: new(CreateRunResponse),
		},
		{
			name: "RunSummary",
			json: `{"run_id":"run-1","state":"active","substate":"in_turn","created_at":"2026-11-19T06:30:00.123Z"}`,
			into: new(RunSummary),
		},
		{
			name: "Turn",
			json: `{"turn_id":"turn-1","started_at":"2026-11-19T06:30:00.000Z","ended_at":"2026-11-19T06:30:05.250Z","output":"line one\nline two","is_error":true}`,
			into: new(Turn),
		},
		{
			name: "Suspension",
			json: `{"started_at":"2026-11-19T06:32:00.000Z","snapshot_ms":12,"restore_ms":null,"hello_ms":34,"error":"boom"}`,
			into: new(Suspension),
		},
		{
			name: "RunDetail",
			json: `{"run_id":"run-1","state":"suspended","substate":"snapshotted","created_at":"2026-11-19T06:30:00.123Z","failure_reason":"","turns":[{"turn_id":"turn-1","started_at":"2026-11-19T06:30:00.000Z","ended_at":"2026-11-19T06:30:05.250Z","output":"line one\nline two","is_error":false},{"turn_id":"turn-2","started_at":"2026-11-19T06:31:00.000Z","ended_at":null,"output":"","is_error":true}],"suspensions":[{"started_at":"2026-11-19T06:32:00.000Z","snapshot_ms":12,"restore_ms":null,"hello_ms":34,"error":"boom"}]}`,
			into: new(RunDetail),
		},
		{
			name: "SendMessageRequest",
			json: `{"text":"hello"}`,
			into: new(SendMessageRequest),
		},
		{
			name: "SendMessageResponse",
			json: `{"inbox_seq":7}`,
			into: new(SendMessageResponse),
		},
		{
			name: "ErrorResponse",
			json: `{"error":"run not found"}`,
			into: new(ErrorResponse),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tt.json), tt.into); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			got, err := json.Marshal(tt.into)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if string(got) != tt.json {
				t.Errorf("round trip = %s, want %s", got, tt.json)
			}
		})
	}
}

package proto

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// TestJobMessages_RoundTrip verifies that every job management message
// survives a marshal/unmarshal round trip, including the additive fields that
// older agents and backends ignore.
func TestJobMessages_RoundTrip(t *testing.T) {
	cases := []proto.Message{
		&JobOptions{
			JobName:     "nightly",
			Limit:       25,
			ExecutionId: "job_0123456789abcdef",
			Offset:      4096,
			MaxBytes:    32768,
		},
		&JobDefinition{
			AppId:               "app-1",
			JobName:             "nightly",
			WorkloadType:        "job",
			Cron:                "0 2 * * *",
			Timezone:            "UTC",
			Command:             []string{"./bin/nightly", "--fast"},
			TimeoutMs:           900000,
			Retries:             2,
			ConcurrencyPolicy:   "forbid",
			CurrentStatus:       "running",
			RunningExecutionId:  "job_0123456789abcdef",
			LastExecutionStatus: "succeeded",
			LastExecutionAt:     1_700_000_000_000,
			NextScheduledAt:     1_700_003_600_000,
		},
		&JobExecutionRecord{
			ExecutionId: "job_0123456789abcdef",
			JobName:     "nightly",
			AppId:       "app-1",
			Trigger:     "manual",
			Status:      "succeeded",
			ScheduledAt: 1_700_000_000_000,
			StartedAt:   1_700_000_000_100,
			FinishedAt:  1_700_000_000_900,
			DurationMs:  800,
			Attempts:    1,
			ExitCode:    0,
			Error:       "",
			ErrorCode:   "",
		},
		&JobLogChunk{
			ExecutionId: "job_0123456789abcdef",
			Offset:      0,
			NextOffset:  42,
			TotalBytes:  42,
			Eof:         true,
			Entries: []*JobLogEntry{{
				Stream:    "stdout",
				Level:     "INFO",
				Message:   "done",
				Timestamp: 1_700_000_000_900,
			}},
		},
		&JobLifecycleEvent{
			ServerId:    "srv-1",
			AppId:       "app-1",
			JobName:     "nightly",
			ExecutionId: "job_0123456789abcdef",
			Trigger:     "scheduled",
			Phase:       "attempt_failed",
			Status:      "running",
			Timestamp:   1_700_000_000_500,
			Attempt:     1,
			Error:       "exit status 1",
			ErrorCode:   "exit_code",
			ScheduledAt: 1_700_000_000_000,
		},
	}

	for _, want := range cases {
		data, err := proto.Marshal(want)
		if err != nil {
			t.Fatalf("marshal %T failed: %v", want, err)
		}

		got := want.ProtoReflect().New().Interface()
		if err := proto.Unmarshal(data, got); err != nil {
			t.Fatalf("unmarshal %T failed: %v", want, err)
		}

		if !proto.Equal(want, got) {
			t.Fatalf("round trip mismatch for %T:\nwant: %v\ngot:  %v", want, want, got)
		}
	}
}

// TestJobWireCompatibility locks the additive field numbers to the backend's
// proto. A mismatch silently drops job payloads on the wire, which a
// round-trip test cannot detect.
func TestJobWireCompatibility(t *testing.T) {
	checks := []struct {
		name   string
		fields protoreflect.FieldDescriptors
		want   map[protoreflect.Name]protoreflect.FieldNumber
	}{
		{
			name:   "MonitorCommandRequest",
			fields: (&MonitorCommandRequest{}).ProtoReflect().Descriptor().Fields(),
			want:   map[protoreflect.Name]protoreflect.FieldNumber{"job": 16},
		},
		{
			name:   "MonitorCommandResult",
			fields: (&MonitorCommandResult{}).ProtoReflect().Descriptor().Fields(),
			want: map[protoreflect.Name]protoreflect.FieldNumber{
				"jobs":       31,
				"execution":  32,
				"executions": 33,
				"job_log":    34,
			},
		},
		{
			name:   "JobOptions",
			fields: (&JobOptions{}).ProtoReflect().Descriptor().Fields(),
			want: map[protoreflect.Name]protoreflect.FieldNumber{
				"job_name":     1,
				"limit":        2,
				"execution_id": 3,
				"offset":       4,
				"max_bytes":    5,
			},
		},
		{
			name:   "JobExecutionRecord",
			fields: (&JobExecutionRecord{}).ProtoReflect().Descriptor().Fields(),
			want: map[protoreflect.Name]protoreflect.FieldNumber{
				"execution_id": 1,
				"job_name":     2,
				"app_id":       3,
				"trigger":      4,
				"status":       5,
				"scheduled_at": 6,
				"started_at":   7,
				"finished_at":  8,
				"duration_ms":  9,
				"attempts":     10,
				"exit_code":    11,
				"error":        12,
				"error_code":   13,
			},
		},
		{
			name:   "JobLogChunk",
			fields: (&JobLogChunk{}).ProtoReflect().Descriptor().Fields(),
			want: map[protoreflect.Name]protoreflect.FieldNumber{
				"execution_id": 1,
				"offset":       2,
				"next_offset":  3,
				"total_bytes":  4,
				"eof":          5,
				"entries":      6,
			},
		},
	}

	for _, check := range checks {
		for name, want := range check.want {
			field := check.fields.ByName(name)
			if field == nil {
				t.Fatalf("%s.%s is missing", check.name, name)
			}
			if got := field.Number(); got != want {
				t.Fatalf("%s.%s = %d, want %d", check.name, name, got, want)
			}
		}
	}

	event := (&MonitorEvent{}).ProtoReflect().Descriptor().Fields().ByName("job_event")
	if event == nil || event.Number() != 26 {
		t.Fatalf("MonitorEvent.job_event field number drift")
	}
}

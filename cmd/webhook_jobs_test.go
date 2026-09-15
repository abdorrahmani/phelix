package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/webhook"
)

func jobFixture(status, stage string, version int) webhook.JobRecord {
	return webhook.JobRecord{
		SchemaVersion: 1,
		ID:            "wh_0011223344556677",
		DeliveryID:    "d-cli-1",
		AppID:         "app-1",
		AppName:       "api",
		Branch:        "main",
		Commit:        "abcdef1234567890abcdef1234567890abcdef12",
		Provider:      webhook.ProviderGitHub,
		Status:        status,
		Stage:         stage,
		AcceptedAt:    time.Now().UnixMilli(),
		Version:       version,
	}
}

func TestRenderWebhookStatusTable(t *testing.T) {
	var buf bytes.Buffer
	recs := []webhook.JobRecord{
		jobFixture(webhook.StatusHealthChecking, webhook.StageHealthCheck, 18),
	}
	renderWebhookStatusTable(&buf, recs)
	out := buf.String()
	for _, want := range []string{"wh_0011223344556677", "health_checking", "health_check", "abcdef1", "v18"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status table missing %q:\n%s", want, out)
		}
	}

	buf.Reset()
	renderWebhookStatusTable(&buf, []webhook.JobRecord{jobFixture(webhook.StatusSyncing, webhook.StageGitSync, 0)})
	if !strings.Contains(buf.String(), "-") {
		t.Fatalf("unset version must render as '-'):\n%s", buf.String())
	}
}

func TestRenderWebhookHistoryTable(t *testing.T) {
	rec := jobFixture(webhook.StatusRolledBack, webhook.StageCleanup, 17)
	rec.FinishedAt = time.Now().UnixMilli()
	rec.ErrorCode = webhook.JobErrHealth
	rec.ErrorMessage = "health check failed"

	var buf bytes.Buffer
	renderWebhookHistoryTable(&buf, []webhook.JobRecord{rec})
	out := buf.String()
	for _, want := range []string{"rolled_back", "health check failed", "v17"} {
		if !strings.Contains(out, want) {
			t.Fatalf("history table missing %q:\n%s", want, out)
		}
	}

	// Succeeded jobs without an error message get a friendly one.
	ok := jobFixture(webhook.StatusSucceeded, webhook.StageCleanup, 18)
	ok.FinishedAt = time.Now().UnixMilli()
	buf.Reset()
	renderWebhookHistoryTable(&buf, []webhook.JobRecord{ok})
	if !strings.Contains(buf.String(), "deployed") {
		t.Fatalf("succeeded history must show a friendly message:\n%s", buf.String())
	}
}

func TestRenderWebhookJobsJSON(t *testing.T) {
	rec := jobFixture(webhook.StatusSucceeded, webhook.StageCleanup, 18)
	rec.FinishedAt = time.Now().UnixMilli()

	var buf bytes.Buffer
	if err := renderWebhookJobsJSON(&buf, []webhook.JobRecord{rec}); err != nil {
		t.Fatal(err)
	}
	var decoded []webhook.JobRecord
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("json output is not valid: %v\n%s", err, buf.String())
	}
	if len(decoded) != 1 || decoded[0].ID != "wh_0011223344556677" || decoded[0].Version != 18 {
		t.Fatalf("decoded = %+v", decoded)
	}
	lower := strings.ToLower(buf.String())
	for _, banned := range []string{"secret", "worktree"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("json output leaks %q:\n%s", banned, buf.String())
		}
	}

	// Empty history renders as an empty JSON array, not null.
	buf.Reset()
	if err := renderWebhookJobsJSON(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) != "[]" {
		t.Fatalf("empty json = %q, want []", buf.String())
	}
}

func TestWebhookCommandTree(t *testing.T) {
	var status, history bool
	for _, sub := range WebhookCmd.Commands() {
		switch sub.Name() {
		case "status":
			status = true
		case "history":
			history = true
		}
	}
	if !status || !history {
		t.Fatal("webhook command must expose status and history subcommands")
	}

	// The daemon parent must not silently swallow unknown arguments.
	if err := WebhookCmd.Args(WebhookCmd, []string{"bogus"}); err == nil {
		t.Fatal("unknown argument must be rejected (it would otherwise start the daemon)")
	}
	if err := WebhookCmd.Args(WebhookCmd, nil); err != nil {
		t.Fatalf("bare 'phelix webhook' must keep starting the daemon: %v", err)
	}
}

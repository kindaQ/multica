package lark

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestSelectSingleAccessibleInstallation(t *testing.T) {
	id := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}

	t.Run("selects the only accessible Bot", func(t *testing.T) {
		got, err := selectSingleAccessibleInstallation([]Installation{{ID: id}})
		if err != nil {
			t.Fatalf("selectSingleAccessibleInstallation() error = %v", err)
		}
		if got.ID != id {
			t.Fatalf("installation id = %v, want %v", got.ID, id)
		}
	})

	t.Run("requires a Bot", func(t *testing.T) {
		_, err := selectSingleAccessibleInstallation(nil)
		if err == nil || !strings.Contains(err.Error(), "no active Feishu Bot") {
			t.Fatalf("error = %v, want no active Bot", err)
		}
	})

	t.Run("refuses an ambiguous automatic choice", func(t *testing.T) {
		_, err := selectSingleAccessibleInstallation([]Installation{{ID: id}, {ID: id}})
		if err == nil || !strings.Contains(err.Error(), "multiple active Feishu Bots") {
			t.Fatalf("error = %v, want multiple Bots", err)
		}
	})
}

func TestFlattenOutboundPost(t *testing.T) {
	raw := json.RawMessage(`{"zh_cn":{"title":"阶段完成","content":[[{"tag":"text","text":"当前任务："},{"tag":"a","text":"PEN-4","href":"http://example.test/issues/4"}],[{"tag":"img","image_key":"img_1"}]]}}`)
	got, err := flattenOutboundPost(raw)
	if err != nil {
		t.Fatalf("flattenOutboundPost() error = %v", err)
	}
	if !strings.Contains(got, "阶段完成") || !strings.Contains(got, "PEN-4 (http://example.test/issues/4)") || !strings.Contains(got, "[Image]") {
		t.Fatalf("flattenOutboundPost() = %q", got)
	}
}

func TestAppendFooterToPost(t *testing.T) {
	raw := json.RawMessage(`{"zh_cn":{"title":"阶段完成","content":[[{"tag":"text","text":"完成"}]]}}`)
	got, err := appendFooterToPost(raw, "from developer (issue PEN-4)")
	if err != nil {
		t.Fatalf("appendFooterToPost() error = %v", err)
	}
	flattened, err := flattenOutboundPost(got)
	if err != nil {
		t.Fatalf("flattenOutboundPost() error = %v", err)
	}
	if !strings.Contains(flattened, "──────────") || !strings.Contains(flattened, "from developer (issue PEN-4)") {
		t.Fatalf("flattened post = %q", flattened)
	}
}

func TestProactiveMessageFooter(t *testing.T) {
	tests := []struct {
		name            string
		agentName       string
		issueIdentifier string
		want            string
	}{
		{name: "issue route", agentName: " developer ", issueIdentifier: "PEN-4", want: "from developer (issue PEN-4)"},
		{name: "chat route", agentName: "tester", want: "from tester"},
		{name: "missing agent name", want: "from agent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := proactiveMessageFooter(tt.agentName, tt.issueIdentifier); got != tt.want {
				t.Fatalf("proactiveMessageFooter() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAppendMessageFooter(t *testing.T) {
	got := appendMessageFooter("正文\n", "from tester")
	want := "正文\n\n──────────\nfrom tester"
	if got != want {
		t.Fatalf("appendMessageFooter() = %q, want %q", got, want)
	}
	if containsMarkdown(got) {
		t.Fatal("the source footer must not turn a plain chat reply into a markdown card")
	}
}

func TestProactiveChatTitle(t *testing.T) {
	if got := proactiveChatTitle("developer"); got != "developer · Feishu proactive chat" {
		t.Fatalf("proactiveChatTitle() = %q", got)
	}
	if got := proactiveChatTitle("  tester  "); got != "tester · Feishu proactive chat" {
		t.Fatalf("proactiveChatTitle() trims name = %q", got)
	}
	if got := proactiveChatTitle("  "); got != "Feishu proactive chat" {
		t.Fatalf("proactiveChatTitle() fallback = %q", got)
	}
}

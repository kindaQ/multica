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

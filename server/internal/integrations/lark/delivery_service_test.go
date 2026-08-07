package lark

import (
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

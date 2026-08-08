package service

import (
	"strings"
	"testing"
)

func TestIsTrivialDoneOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"plain english", "done", true},
		{"english punctuation", " Done. ", true},
		{"no reply needed", "No reply needed.", true},
		{"no response needed", "No response needed", true},
		{"pure acknowledgment", "Pure acknowledgment — no work produced this turn, so no reply warranted.", true},
		{"pure acknowledgment variant", "Pure acknowledgment again — no work needed, no reply warranted.", true},
		{"no action variant", "No action is required, so no response is needed.", true},
		{"russian", "Готово!", true},
		{"russian feminine", "готова…", true},
		{"russian done", "Сделано", true},
		{"chinese", "完成！", true},
		{"japanese", "完了。", true},
		{"not only marker", "done, see PR", false},
		{"not acknowledgement", "好的", false},
		{"real no reply answer", "No reply needed from the API client; use the webhook instead.", false},
		{"long explanation", "No work was produced and no reply is warranted. " + strings.Repeat("context ", 50), false},
		{"real answer", "I fixed the issue", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTrivialDoneOutput(tt.in); got != tt.want {
				t.Fatalf("isTrivialDoneOutput(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestReactionEmojiForOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		output    string
		wantEmoji string
		want      bool
	}{
		{name: "model-selected emoji", output: "REACTION: 😂", wantEmoji: "😂", want: true},
		{name: "variation selector", output: "reaction: ❤️", wantEmoji: "❤️", want: true},
		{name: "legacy no-reply output", output: "Pure acknowledgment again — no work needed, no reply warranted.", wantEmoji: "👍", want: true},
		{name: "reject words", output: "REACTION: acknowledged", want: false},
		{name: "reject non-emoji text", output: "REACTION: 好的", want: false},
		{name: "reject explanation", output: "REACTION: 😂 thanks", want: false},
		{name: "real answer", output: "I fixed the issue", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			emoji, ok := reactionEmojiForOutput(tt.output)
			if ok != tt.want || emoji != tt.wantEmoji {
				t.Fatalf("reactionEmojiForOutput(%q) = (%q, %v), want (%q, %v)", tt.output, emoji, ok, tt.wantEmoji, tt.want)
			}
		})
	}
}

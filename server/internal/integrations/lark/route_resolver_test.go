package lark

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

func TestParseRouteCommand(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  routeCommand
		bad   bool
	}{
		{name: "help by default", input: "/route", want: routeCommand{Action: "help"}},
		{name: "status", input: "/route status", want: routeCommand{Action: "status"}},
		{name: "cancel case insensitive", input: "/ROUTE CANCEL", want: routeCommand{Action: "cancel"}},
		{name: "issue and agent", input: "/route --issue MUL-42 --agent @Writer", want: routeCommand{Action: "set", Issue: "MUL-42", Agent: "@Writer"}},
		{name: "agent only", input: "/route --agent 00000000-0000-0000-0000-000000000001", want: routeCommand{Action: "set", Agent: "00000000-0000-0000-0000-000000000001"}},
		{name: "missing value", input: "/route --issue", bad: true},
		{name: "unknown flag", input: "/route --team x", bad: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseRouteCommand(test.input)
			if test.bad {
				if err == nil {
					t.Fatalf("expected error, got %#v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != test.want {
				t.Fatalf("got %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestRouteConversationKeyScopesTopics(t *testing.T) {
	base := channel.InboundMessage{Source: channel.Source{ChatID: "oc_group"}}
	if got := routeConversationKey(base); got != "oc_group" {
		t.Fatalf("plain key = %q", got)
	}
	base.Source.ThreadID = "omt_topic"
	if got := routeConversationKey(base); got != "oc_group:omt_topic" {
		t.Fatalf("topic key = %q", got)
	}
}

func TestCommentDeliveryPayloadAcceptsPointerTaskID(t *testing.T) {
	taskID := "00000000-0000-0000-0000-000000000002"
	comment, ok := commentDeliveryPayload(map[string]any{"comment": map[string]any{
		"id": "00000000-0000-0000-0000-000000000001", "source_task_id": &taskID,
		"author_type": "agent", "type": "comment", "content": "done",
	}})
	if !ok || !comment.taskID.Valid || comment.content != "done" {
		t.Fatalf("unexpected parsed comment: %#v, ok=%v", comment, ok)
	}
}

package lark

import (
	"net/url"
	"reflect"
	"testing"
)

func TestDispatchTargetMentions(t *testing.T) {
	mentions := []InboundMention{
		{OpenID: "ou_bot"},
		{OpenID: "ou_target"},
		{OpenID: "ou_target"},
		{OpenID: ""},
	}
	if got, want := dispatchTargetMentions(mentions, "ou_bot"), []string{"ou_target"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatchTargetMentions() = %v, want %v", got, want)
	}
}

func TestOutboundMessageRequestUsesOpenIDForPrivateDelivery(t *testing.T) {
	path, body := outboundMessageRequest("", "ou_recipient", "text", `{"text":"done"}`, ReplyTarget{})
	parsed, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Query().Get("receive_id_type"); got != "open_id" {
		t.Fatalf("receive_id_type = %q, want open_id", got)
	}
	if got := body["receive_id"]; got != "ou_recipient" {
		t.Fatalf("receive_id = %v, want ou_recipient", got)
	}
}

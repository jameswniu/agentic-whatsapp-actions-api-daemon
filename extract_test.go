package main

import (
	"testing"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestExtractText(t *testing.T) {
	cases := []struct {
		name string
		msg  *waE2E.Message
		want string
	}{
		{"nil", nil, ""},
		{"plain", &waE2E.Message{Conversation: proto.String("hi there")}, "hi there"},
		{"extended", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("link msg")}}, "link msg"},
		{"image no caption trims trailing space",
			&waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}, "[image]"},
		{"image with caption",
			&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("a diagram")}}, "[image] a diagram"},
		{"video with duration + caption",
			&waE2E.Message{VideoMessage: &waE2E.VideoMessage{Seconds: proto.Uint32(12), Caption: proto.String("clip")}}, "[video 12s] clip"},
		{"audio no ptt",
			&waE2E.Message{AudioMessage: &waE2E.AudioMessage{Seconds: proto.Uint32(5)}}, "[audio 5s]"},
		{"voice note ptt",
			&waE2E.Message{AudioMessage: &waE2E.AudioMessage{Seconds: proto.Uint32(8), PTT: proto.Bool(true)}}, "[voice note 8s]"},
		{"document filename",
			&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("spec.pdf")}}, "[document] spec.pdf"},
		{"sticker", &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}, "[sticker]"},
		{"location named",
			&waE2E.Message{LocationMessage: &waE2E.LocationMessage{Name: proto.String("Blue Bottle")}}, "[location] Blue Bottle"},
		{"contact",
			&waE2E.Message{ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String("Jane Doe")}}, "[contact] Jane Doe"},
		{"reaction",
			&waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}}, "[reaction] 👍"},
		{"poll",
			&waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{Name: proto.String("Lunch?")}}, "[poll] Lunch?"},
		{"revoke -> deleted",
			&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum()}}, "[deleted]"},
		{"device-sent unwrap",
			&waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: &waE2E.Message{Conversation: proto.String("self note")}}}, "self note"},
		{"view-once unwrap",
			&waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("secret")}}}}, "[view-once] [image] secret"},
	}
	for _, c := range cases {
		if got := extractText(c.msg); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

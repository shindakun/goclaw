package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestMapAttachments(t *testing.T) {
	t.Run("no attachments leaves text and yields nil", func(t *testing.T) {
		text, atts := mapAttachments("hi", nil)
		if text != "hi" || atts != nil {
			t.Fatalf("text=%q atts=%v", text, atts)
		}
	})

	t.Run("typed placeholders appended to text", func(t *testing.T) {
		in := []*discordgo.MessageAttachment{
			{Filename: "cat.png", ContentType: "image/png", URL: "https://x/cat.png"},
			{Filename: "clip.mp4", ContentType: "video/mp4"},
			{Filename: "song.mp3", ContentType: "audio/mpeg"},
			{Filename: "notes.pdf", ContentType: "application/pdf"},
		}
		text, atts := mapAttachments("look", in)
		want := "look\n[Image: cat.png]\n[Video: clip.mp4]\n[Audio: song.mp3]\n[File: notes.pdf]"
		if text != want {
			t.Fatalf("text=\n%q\nwant\n%q", text, want)
		}
		if len(atts) != 4 {
			t.Fatalf("atts = %d, want 4", len(atts))
		}
		if atts[0].Filename != "cat.png" || atts[0].MIMEType != "image/png" || atts[0].URL != "https://x/cat.png" {
			t.Fatalf("attachment 0 not mapped: %+v", atts[0])
		}
	})

	t.Run("attachment with empty text becomes the placeholders", func(t *testing.T) {
		in := []*discordgo.MessageAttachment{{Filename: "a.bin", ContentType: ""}}
		text, atts := mapAttachments("", in)
		if text != "[File: a.bin]" || len(atts) != 1 {
			t.Fatalf("text=%q atts=%d", text, len(atts))
		}
	})

	t.Run("missing filename falls back to a generic name", func(t *testing.T) {
		in := []*discordgo.MessageAttachment{{ContentType: "image/jpeg"}}
		text, _ := mapAttachments("", in)
		if text != "[Image: image]" {
			t.Fatalf("text=%q", text)
		}
	})
}

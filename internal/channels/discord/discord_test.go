package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestStripSelfMention(t *testing.T) {
	const id = "123"
	cases := []struct {
		name    string
		content string
		selfID  string
		want    string
	}{
		{"leading mention before slash command", "<@123> /commands", id, "/commands"},
		{"nickname mention form", "<@!123> /roll 2d6", id, "/roll 2d6"},
		{"mention with extra spaces", "<@123>   /roll 2d6", id, "/roll 2d6"},
		{"mention mid text", "hey <@123> roll please", id, "hey  roll please"},
		{"no mention is unchanged", "/commands", id, "/commands"},
		{"other user's mention is kept", "<@999> hi", id, "<@999> hi"},
		{"empty selfID leaves content", "<@123> /commands", "", "<@123> /commands"},
		{"only a mention becomes empty", "<@123>", id, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripSelfMention(c.content, c.selfID); got != c.want {
				t.Fatalf("stripSelfMention(%q, %q) = %q, want %q", c.content, c.selfID, got, c.want)
			}
		})
	}
}

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
			{Filename: "clip.mp4", ContentType: "video/mp4", URL: "https://x/clip.mp4"},
			{Filename: "song.mp3", ContentType: "audio/mpeg", URL: "https://x/song.mp3"},
			{Filename: "notes.pdf", ContentType: "application/pdf", URL: "https://x/notes.pdf"},
			{Filename: "data.bin", ContentType: "", URL: "https://x/data.bin"}, // unknown type -> File
		}
		text, atts := mapAttachments("look", in)
		want := "look\n[Image: cat.png]\n[Video: clip.mp4]\n[Audio: song.mp3]\n[File: notes.pdf]\n[File: data.bin]"
		if text != want {
			t.Fatalf("text=\n%q\nwant\n%q", text, want)
		}
		if len(atts) != 5 {
			t.Fatalf("atts = %d, want 5", len(atts))
		}
		// Every attachment's filename, MIME, and URL must round-trip in order.
		wantAtts := []struct{ name, mime, url string }{
			{"cat.png", "image/png", "https://x/cat.png"},
			{"clip.mp4", "video/mp4", "https://x/clip.mp4"},
			{"song.mp3", "audio/mpeg", "https://x/song.mp3"},
			{"notes.pdf", "application/pdf", "https://x/notes.pdf"},
			{"data.bin", "", "https://x/data.bin"},
		}
		for i, w := range wantAtts {
			if atts[i].Filename != w.name || atts[i].MIMEType != w.mime || atts[i].URL != w.url {
				t.Fatalf("attachment %d = %+v, want %v", i, atts[i], w)
			}
		}
	})

	t.Run("non-media MIME type renders as File", func(t *testing.T) {
		in := []*discordgo.MessageAttachment{{Filename: "x.zip", ContentType: "application/zip"}}
		text, _ := mapAttachments("", in)
		if text != "[File: x.zip]" {
			t.Fatalf("text=%q, want [File: x.zip]", text)
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

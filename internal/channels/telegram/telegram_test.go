package telegram

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/shindakun/goclaw/internal/channels"
)

func TestMapAttachments(t *testing.T) {
	t.Run("plain text has no attachments", func(t *testing.T) {
		text, atts := mapAttachments("hello", &tgbotapi.Message{})
		if text != "hello" || atts != nil {
			t.Fatalf("text=%q atts=%v", text, atts)
		}
	})

	t.Run("photo appends placeholder and uses largest size", func(t *testing.T) {
		m := &tgbotapi.Message{
			Photo: []tgbotapi.PhotoSize{
				{FileID: "small"},
				{FileID: "large"},
			},
		}
		text, atts := mapAttachments("look", m)
		if text != "look\n[Image]" {
			t.Fatalf("text=%q", text)
		}
		if len(atts) != 1 || atts[0].URL != "large" {
			t.Fatalf("expected largest photo file_id carried, got %+v", atts)
		}
	})

	t.Run("document keeps filename and mime", func(t *testing.T) {
		m := &tgbotapi.Message{
			Document: &tgbotapi.Document{FileID: "doc1", FileName: "report.pdf", MimeType: "application/pdf"},
		}
		text, atts := mapAttachments("", m)
		if text != "[File: report.pdf]" {
			t.Fatalf("text=%q", text)
		}
		if atts[0].Filename != "report.pdf" || atts[0].MIMEType != "application/pdf" || atts[0].URL != "doc1" {
			t.Fatalf("doc not mapped: %+v", atts[0])
		}
	})

	t.Run("document without filename falls back", func(t *testing.T) {
		m := &tgbotapi.Message{Document: &tgbotapi.Document{FileID: "d"}}
		text, _ := mapAttachments("", m)
		if text != "[File: file]" {
			t.Fatalf("text=%q", text)
		}
	})

	t.Run("voice placeholder", func(t *testing.T) {
		m := &tgbotapi.Message{Voice: &tgbotapi.Voice{FileID: "v", MimeType: "audio/ogg"}}
		text, atts := mapAttachments("", m)
		if text != "[Voice]" || atts[0].MIMEType != "audio/ogg" {
			t.Fatalf("text=%q atts=%+v", text, atts)
		}
	})

	t.Run("video placeholder carries fileid and mime", func(t *testing.T) {
		m := &tgbotapi.Message{Video: &tgbotapi.Video{FileID: "vid", FileName: "clip.mp4", MimeType: "video/mp4"}}
		text, atts := mapAttachments("", m)
		if text != "[Video]" {
			t.Fatalf("text=%q", text)
		}
		if atts[0].URL != "vid" || atts[0].Filename != "clip.mp4" || atts[0].MIMEType != "video/mp4" {
			t.Fatalf("video not mapped: %+v", atts[0])
		}
	})

	t.Run("audio with name", func(t *testing.T) {
		m := &tgbotapi.Message{Audio: &tgbotapi.Audio{FileID: "au", FileName: "song.mp3", MimeType: "audio/mpeg"}}
		text, atts := mapAttachments("", m)
		if text != "[Audio: song.mp3]" || atts[0].URL != "au" {
			t.Fatalf("text=%q atts=%+v", text, atts)
		}
	})

	t.Run("audio without name falls back", func(t *testing.T) {
		m := &tgbotapi.Message{Audio: &tgbotapi.Audio{FileID: "au"}}
		text, _ := mapAttachments("", m)
		if text != "[Audio: audio]" {
			t.Fatalf("text=%q", text)
		}
	})

	t.Run("sticker placeholder", func(t *testing.T) {
		m := &tgbotapi.Message{Sticker: &tgbotapi.Sticker{FileID: "st"}}
		text, atts := mapAttachments("", m)
		if text != "[Sticker]" || atts[0].URL != "st" {
			t.Fatalf("text=%q atts=%+v", text, atts)
		}
	})

	// The mapping is a switch: when more than one media field is set, Photo wins
	// (it is the first case). Locks in that precedence so a reorder is caught.
	t.Run("photo takes precedence over a co-present document", func(t *testing.T) {
		m := &tgbotapi.Message{
			Photo:    []tgbotapi.PhotoSize{{FileID: "p"}},
			Document: &tgbotapi.Document{FileID: "d", FileName: "x.pdf"},
		}
		text, atts := mapAttachments("", m)
		if text != "[Image]" || len(atts) != 1 || atts[0].URL != "p" {
			t.Fatalf("expected photo to win, got text=%q atts=%+v", text, atts)
		}
	})

	t.Run("caption is preserved as text alongside media", func(t *testing.T) {
		// The caller passes the caption in as text; mapAttachments appends media.
		m := &tgbotapi.Message{Photo: []tgbotapi.PhotoSize{{FileID: "p"}}}
		text, _ := mapAttachments("my caption", m)
		if text != "my caption\n[Image]" {
			t.Fatalf("text=%q", text)
		}
	})
}

func TestSenderIDAndName(t *testing.T) {
	t.Run("nil From yields empty", func(t *testing.T) {
		m := &tgbotapi.Message{}
		if senderID(m) != "" || senderName(m) != "" {
			t.Fatalf("expected empties for nil From")
		}
	})

	t.Run("username preferred, prefixed with @", func(t *testing.T) {
		m := &tgbotapi.Message{From: &tgbotapi.User{ID: 42, UserName: "steve", FirstName: "Steve"}}
		if got := senderID(m); got != "42" {
			t.Fatalf("senderID = %q, want 42", got)
		}
		if got := senderName(m); got != "@steve" {
			t.Fatalf("senderName = %q, want @steve", got)
		}
	})

	t.Run("falls back to first name without username", func(t *testing.T) {
		m := &tgbotapi.Message{From: &tgbotapi.User{ID: 7, FirstName: "Steve"}}
		if got := senderName(m); got != "Steve" {
			t.Fatalf("senderName = %q, want Steve", got)
		}
	})
}

func TestResolveAttachments(t *testing.T) {
	t.Run("resolves file ids to direct urls", func(t *testing.T) {
		a := &Adapter{resolveFileURL: func(fileID string) (string, error) {
			return "https://api.telegram.org/file/bot<token>/" + fileID, nil
		}}
		atts := []channels.Attachment{{URL: "doc1"}, {URL: "photo2"}}
		a.resolveAttachments(atts)
		if atts[0].URL != "https://api.telegram.org/file/bot<token>/doc1" {
			t.Fatalf("att0 url = %q", atts[0].URL)
		}
		if atts[1].URL != "https://api.telegram.org/file/bot<token>/photo2" {
			t.Fatalf("att1 url = %q", atts[1].URL)
		}
	})

	t.Run("a resolve failure clears that url but does not panic or affect others", func(t *testing.T) {
		a := &Adapter{resolveFileURL: func(fileID string) (string, error) {
			if fileID == "bad" {
				return "", errFake
			}
			return "url:" + fileID, nil
		}}
		atts := []channels.Attachment{{URL: "good"}, {URL: "bad"}}
		a.resolveAttachments(atts)
		if atts[0].URL != "url:good" {
			t.Fatalf("good att url = %q", atts[0].URL)
		}
		if atts[1].URL != "" {
			t.Fatalf("failed att url should be cleared, got %q", atts[1].URL)
		}
	})

	t.Run("nil resolver is a no-op", func(t *testing.T) {
		a := &Adapter{}
		atts := []channels.Attachment{{URL: "keep"}}
		a.resolveAttachments(atts)
		if atts[0].URL != "keep" {
			t.Fatalf("nil resolver must leave url untouched, got %q", atts[0].URL)
		}
	})
}

var errFake = fmtError("telegram: fake resolve failure")

type fmtError string

func (e fmtError) Error() string { return string(e) }

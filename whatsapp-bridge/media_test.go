package main

import (
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestExtractTextContent(t *testing.T) {
	tests := []struct {
		name string
		msg  *waE2E.Message
		want string
	}{
		{"nil", nil, ""},
		{"empty", &waE2E.Message{}, ""},
		{"conversation", &waE2E.Message{Conversation: proto.String("hello")}, "hello"},
		{"extended", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("ext")}}, "ext"},
		{"image only", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractTextContent(tt.msg); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestExtractMediaInfo(t *testing.T) {
	key, sha, enc := []byte{1}, []byte{2}, []byte{3}
	tests := []struct {
		name     string
		msg      *waE2E.Message
		wantType string
		wantName string // exact name, or prefix when wantPfx is set
		wantPfx  bool
		wantURL  string
		wantLen  uint64
	}{
		{"nil", nil, "", "", false, "", 0},
		{"text only", &waE2E.Message{Conversation: proto.String("x")}, "", "", false, "", 0},
		{"image", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			URL: proto.String("u-img"), MediaKey: key, FileSHA256: sha, FileEncSHA256: enc, FileLength: proto.Uint64(10)}},
			"image", "image_", true, "u-img", 10},
		{"video", &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			URL: proto.String("u-vid"), MediaKey: key, FileSHA256: sha, FileEncSHA256: enc, FileLength: proto.Uint64(20)}},
			"video", "video_", true, "u-vid", 20},
		{"audio", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			URL: proto.String("u-aud"), MediaKey: key, FileSHA256: sha, FileEncSHA256: enc, FileLength: proto.Uint64(30)}},
			"audio", "audio_", true, "u-aud", 30},
		{"document named", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			URL: proto.String("u-doc"), FileName: proto.String("report.pdf"), MediaKey: key, FileSHA256: sha, FileEncSHA256: enc, FileLength: proto.Uint64(40)}},
			"document", "report.pdf", false, "u-doc", 40},
		{"document unnamed", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{URL: proto.String("u-doc2")}},
			"document", "document_", true, "u-doc2", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mt, name, url, k, h, e, l := extractMediaInfo(tt.msg)
			if mt != tt.wantType {
				t.Fatalf("type = %q want %q", mt, tt.wantType)
			}
			if tt.wantPfx {
				if !strings.HasPrefix(name, tt.wantName) {
					t.Fatalf("filename %q lacks prefix %q", name, tt.wantName)
				}
			} else if name != tt.wantName {
				t.Fatalf("filename = %q want %q", name, tt.wantName)
			}
			if url != tt.wantURL || l != tt.wantLen {
				t.Fatalf("url=%q len=%d, want %q %d", url, l, tt.wantURL, tt.wantLen)
			}
			if tt.wantType != "" && tt.wantLen != 0 {
				if string(k) != string(key) || string(h) != string(sha) || string(e) != string(enc) {
					t.Fatalf("key material not propagated: %v %v %v", k, h, e)
				}
			}
		})
	}
}

func TestExtractDirectPathFromURL(t *testing.T) {
	tests := []struct {
		name, url, want string
	}{
		{"with query", "https://mmg.whatsapp.net/v/t62.7118-24/123_n.enc?ccb=11-4&oh=abc", "/v/t62.7118-24/123_n.enc"},
		{"no query", "https://mmg.whatsapp.net/v/t62.7118-24/123_n.enc", "/v/t62.7118-24/123_n.enc"},
		{"malformed returns input", "not-a-whatsapp-url", "not-a-whatsapp-url"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractDirectPathFromURL(tt.url); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
)

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSendWhatsAppMessageJIDRecipient(t *testing.T) {
	fc := &fakeClient{connected: true}
	ok, msg := sendWhatsAppMessage(fc, "120363@g.us", "hello", "")
	if !ok || msg != "Message sent to 120363@g.us" {
		t.Fatalf("ok=%v msg=%q", ok, msg)
	}
	if len(fc.sends) != 1 || fc.sends[0].User != "120363" || fc.sends[0].Server != "g.us" {
		t.Fatalf("sends = %v", fc.sends)
	}
	if fc.msgs[0].GetConversation() != "hello" {
		t.Fatalf("conversation = %q", fc.msgs[0].GetConversation())
	}
}

func TestSendWhatsAppMessageBadJID(t *testing.T) {
	fc := &fakeClient{connected: true}
	ok, msg := sendWhatsAppMessage(fc, "1.2.3@s.whatsapp.net", "hi", "")
	if ok || !strings.HasPrefix(msg, "Error parsing JID:") {
		t.Fatalf("ok=%v msg=%q", ok, msg)
	}
	if len(fc.sends) != 0 {
		t.Fatal("nothing must be sent for a bad JID")
	}
}

func TestSendWhatsAppMessageSendError(t *testing.T) {
	fc := &fakeClient{connected: true, sendErr: errors.New("socket closed")}
	ok, msg := sendWhatsAppMessage(fc, "1555", "hi", "")
	if ok || msg != "Error sending message: socket closed" {
		t.Fatalf("ok=%v msg=%q", ok, msg)
	}
}

func TestSendWhatsAppMessageMediaKinds(t *testing.T) {
	oggData, err := os.ReadFile("testdata/sample-3s.ogg")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		file  string
		data  []byte
		mt    whatsmeow.MediaType
		mime  string
		check func(t *testing.T, fc *fakeClient)
	}{
		{"a.jpg", []byte("x"), whatsmeow.MediaImage, "image/jpeg", nil},
		{"a.JPEG", []byte("x"), whatsmeow.MediaImage, "image/jpeg", nil},
		{"a.png", []byte("x"), whatsmeow.MediaImage, "image/png", nil},
		{"a.gif", []byte("x"), whatsmeow.MediaImage, "image/gif", nil},
		{"a.webp", []byte("x"), whatsmeow.MediaImage, "image/webp", nil},
		{"a.mp4", []byte("x"), whatsmeow.MediaVideo, "video/mp4", nil},
		{"a.avi", []byte("x"), whatsmeow.MediaVideo, "video/avi", nil},
		{"a.mov", []byte("x"), whatsmeow.MediaVideo, "video/quicktime", nil},
		{"a.pdf", []byte("x"), whatsmeow.MediaDocument, "application/octet-stream", nil},
		{"a.ogg", oggData, whatsmeow.MediaAudio, "audio/ogg; codecs=opus", func(t *testing.T, fc *fakeClient) {
			a := fc.msgs[0].GetAudioMessage()
			if a.GetSeconds() != 3 || !a.GetPTT() || len(a.GetWaveform()) != 64 {
				t.Fatalf("audio message = seconds %d ptt %v waveform %d", a.GetSeconds(), a.GetPTT(), len(a.GetWaveform()))
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			fc := &fakeClient{connected: true}
			p := writeTemp(t, tc.file, tc.data)
			ok, msg := sendWhatsAppMessage(fc, "1555", "cap", p)
			if !ok {
				t.Fatalf("send failed: %s", msg)
			}
			if len(fc.uploadTypes) != 1 || fc.uploadTypes[0] != tc.mt {
				t.Fatalf("upload types = %v, want %v", fc.uploadTypes, tc.mt)
			}
			m := fc.msgs[0]
			var gotMime, gotCaption string
			switch tc.mt {
			case whatsmeow.MediaImage:
				gotMime, gotCaption = m.GetImageMessage().GetMimetype(), m.GetImageMessage().GetCaption()
			case whatsmeow.MediaVideo:
				gotMime, gotCaption = m.GetVideoMessage().GetMimetype(), m.GetVideoMessage().GetCaption()
			case whatsmeow.MediaDocument:
				d := m.GetDocumentMessage()
				gotMime, gotCaption = d.GetMimetype(), d.GetCaption()
				if d.GetTitle() != tc.file {
					t.Fatalf("title = %q, want %q", d.GetTitle(), tc.file)
				}
			case whatsmeow.MediaAudio:
				gotMime = m.GetAudioMessage().GetMimetype()
				gotCaption = "cap" // audio messages carry no caption
			}
			if gotMime != tc.mime || gotCaption != "cap" {
				t.Fatalf("mime=%q caption=%q", gotMime, gotCaption)
			}
			if tc.check != nil {
				tc.check(t, fc)
			}
		})
	}
}

func TestSendWhatsAppMessageMediaErrors(t *testing.T) {
	t.Run("unreadable path", func(t *testing.T) {
		fc := &fakeClient{connected: true}
		ok, msg := sendWhatsAppMessage(fc, "1", "", filepath.Join(t.TempDir(), "gone.png"))
		if ok || !strings.Contains(msg, "media_path is not accessible") {
			t.Fatalf("ok=%v msg=%q", ok, msg)
		}
	})
	t.Run("upload fails", func(t *testing.T) {
		fc := &fakeClient{connected: true, uploadErr: errors.New("cdn down")}
		ok, msg := sendWhatsAppMessage(fc, "1", "", writeTemp(t, "a.png", []byte("x")))
		if ok || msg != "Error uploading media: cdn down" || len(fc.sends) != 0 {
			t.Fatalf("ok=%v msg=%q sends=%d", ok, msg, len(fc.sends))
		}
	})
	t.Run("not an ogg", func(t *testing.T) {
		fc := &fakeClient{connected: true}
		ok, msg := sendWhatsAppMessage(fc, "1", "", writeTemp(t, "a.ogg", []byte("not ogg at all")))
		if ok || !strings.HasPrefix(msg, "Failed to analyze Ogg Opus file:") || len(fc.sends) != 0 {
			t.Fatalf("ok=%v msg=%q sends=%d", ok, msg, len(fc.sends))
		}
	})
}

func TestHandleSendReportsSendFailure(t *testing.T) {
	fc := &fakeClient{connected: true, sendErr: errors.New("boom")}
	_, h := newTestAPI(t, fc, "")
	rec := do(h, http.MethodPost, "/api/send", `{"recipient":"1","message":"hi"}`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "Error sending message: boom") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleSendMethodAndRecipientValidation(t *testing.T) {
	_, h := newTestAPI(t, &fakeClient{connected: true}, "")
	if rec := do(h, http.MethodGet, "/api/send", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET code = %d", rec.Code)
	}
}

// failingFile is a mediaFile whose Stat or Read can be made to fail.
type failingFile struct {
	statErr error
	size    int64
	r       io.Reader
}

func (f *failingFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *failingFile) Close() error               { return nil }
func (f *failingFile) Stat() (os.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	return fakeInfo{size: f.size}, nil
}

type fakeInfo struct {
	os.FileInfo
	size int64
}

func (i fakeInfo) Mode() os.FileMode { return 0o644 }
func (i fakeInfo) Size() int64       { return i.size }

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func withOpenMediaFile(t *testing.T, f mediaFile) {
	t.Helper()
	old := openMediaFile
	openMediaFile = func(string) (mediaFile, error) { return f, nil }
	t.Cleanup(func() { openMediaFile = old })
}

func TestReadMediaFileFailureModes(t *testing.T) {
	t.Run("open fails", func(t *testing.T) {
		_, err := readMediaFile(filepath.Join(t.TempDir(), "gone.bin"))
		if err == nil || !strings.Contains(err.Error(), "media_path is not accessible") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("stat fails", func(t *testing.T) {
		withOpenMediaFile(t, &failingFile{statErr: errors.New("stat boom")})
		_, err := readMediaFile("/x/y.bin")
		if err == nil || !strings.Contains(err.Error(), "stat boom") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("read fails", func(t *testing.T) {
		withOpenMediaFile(t, &failingFile{size: 3, r: errReader{errors.New("io boom")}})
		_, err := readMediaFile("/x/y.bin")
		if err == nil || !strings.Contains(err.Error(), "io boom") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("file grows past the limit after stat", func(t *testing.T) {
		old := maxMediaBytes
		maxMediaBytes = 4
		t.Cleanup(func() { maxMediaBytes = old })
		withOpenMediaFile(t, &failingFile{size: 2, r: strings.NewReader("0123456789")})
		_, err := readMediaFile("/x/y.bin")
		if err == nil || !strings.Contains(err.Error(), "more than 4 bytes") {
			t.Fatalf("err = %v", err)
		}
	})
}

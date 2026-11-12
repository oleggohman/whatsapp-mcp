package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

func TestMediaDownloaderGetters(t *testing.T) {
	d := &MediaDownloader{
		URL: "u", DirectPath: "/p", MediaKey: []byte{1}, FileLength: 7,
		FileSHA256: []byte{2}, FileEncSHA256: []byte{3}, MediaType: whatsmeow.MediaVideo,
	}
	if d.GetURL() != "u" || d.GetDirectPath() != "/p" || d.GetFileLength() != 7 || d.GetMediaType() != whatsmeow.MediaVideo {
		t.Fatalf("scalar getters wrong: %+v", d)
	}
	if string(d.GetMediaKey()) != "\x01" || string(d.GetFileSHA256()) != "\x02" || string(d.GetFileEncSHA256()) != "\x03" {
		t.Fatalf("byte getters wrong: %+v", d)
	}
}

const dlChat = "1234@s.whatsapp.net"

// seedTyped stores a message of the given media type with complete download info.
func seedTyped(t *testing.T, s *MessageStore, id, mediaType, filename, url string, length uint64) {
	t.Helper()
	if err := s.StoreChat(dlChat, "c", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage(id, dlChat, "sender", "", time.Now(), false, mediaType, filename, url,
		[]byte{1}, []byte{2}, []byte{3}, length); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadMediaTypes(t *testing.T) {
	want := map[string]whatsmeow.MediaType{
		"image": whatsmeow.MediaImage, "video": whatsmeow.MediaVideo,
		"audio": whatsmeow.MediaAudio, "document": whatsmeow.MediaDocument,
	}
	for mt, wm := range want {
		t.Run(mt, func(t *testing.T) {
			root := t.TempDir()
			s := newTestStoreIn(t, root)
			seedTyped(t, s, "m1", mt, "f.bin", "https://mmg.whatsapp.net/v/t62/abc.enc?x=1", 9)
			fc := &fakeClient{connected: true, data: []byte("payload")}
			ok, gotType, name, path, err := downloadMedia(fc, s, "m1", dlChat)
			if err != nil || !ok || gotType != mt || name != "f.bin" {
				t.Fatalf("ok=%v type=%q name=%q err=%v", ok, gotType, name, err)
			}
			assertUnder(t, root, path)
			if b, err := os.ReadFile(path); err != nil || string(b) != "payload" {
				t.Fatalf("file = %q, %v", b, err)
			}
			dl, isDL := fc.dlMsgs[0].(*MediaDownloader)
			if !isDL || dl.MediaType != wm || dl.DirectPath != "/v/t62/abc.enc" || dl.FileLength != 9 {
				t.Fatalf("downloader = %+v", fc.dlMsgs[0])
			}
			// A second call finds the file and does not download again
			if ok, _, _, _, err := downloadMedia(fc, s, "m1", dlChat); !ok || err != nil || fc.downloads != 1 {
				t.Fatalf("second call: ok=%v err=%v downloads=%d", ok, err, fc.downloads)
			}
		})
	}
}

func TestDownloadMediaErrors(t *testing.T) {
	attempt := func(t *testing.T, fc *fakeClient, s *MessageStore, id string) error {
		t.Helper()
		ok, _, _, _, err := downloadMedia(fc, s, id, dlChat)
		if ok {
			t.Fatal("download must fail")
		}
		if err == nil {
			t.Fatal("expected an error")
		}
		return err
	}
	const url = "https://mmg.whatsapp.net/v/t62/abc.enc"

	t.Run("unknown message", func(t *testing.T) {
		s := newTestStore(t)
		if err := attempt(t, &fakeClient{}, s, "none"); !strings.Contains(err.Error(), "failed to find message") {
			t.Fatal(err)
		}
	})
	t.Run("not a media message", func(t *testing.T) {
		s := newTestStore(t)
		if err := s.StoreChat(dlChat, "c", time.Now()); err != nil {
			t.Fatal(err)
		}
		putMsg(t, s, "txt", dlChat, "x", "hello", time.Now())
		if err := attempt(t, &fakeClient{}, s, "txt"); !strings.Contains(err.Error(), "not a media message") {
			t.Fatal(err)
		}
	})
	t.Run("fallback row without media info", func(t *testing.T) {
		// A NULL url makes the extended query fail; the basic query still finds the media type.
		s := newTestStore(t)
		if err := s.StoreChat(dlChat, "c", time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename) VALUES ('m', ?, 's', '', ?, 0, 'image', 'a.jpg')", dlChat, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := attempt(t, &fakeClient{}, s, "m"); !strings.Contains(err.Error(), "incomplete media information") {
			t.Fatal(err)
		}
	})
	t.Run("fallback row that is not media", func(t *testing.T) {
		s := newTestStore(t)
		if err := s.StoreChat(dlChat, "c", time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename) VALUES ('m', ?, 's', 'x', ?, 0, '', '')", dlChat, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := attempt(t, &fakeClient{}, s, "m"); !strings.Contains(err.Error(), "not a media message") {
			t.Fatal(err)
		}
	})
	t.Run("invalid filename", func(t *testing.T) {
		s := newTestStore(t)
		seedTyped(t, s, "m", "image", "..", url, 9)
		if err := attempt(t, &fakeClient{}, s, "m"); !strings.Contains(err.Error(), "invalid media filename") {
			t.Fatal(err)
		}
	})
	t.Run("chat directory cannot be created", func(t *testing.T) {
		root := t.TempDir()
		s := newTestStoreIn(t, root)
		seedTyped(t, s, "m", "image", "a.jpg", url, 9)
		// A regular file where the chat directory must go
		if err := os.WriteFile(filepath.Join(root, sanitizeChatDirName(dlChat)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := attempt(t, &fakeClient{}, s, "m"); !strings.Contains(err.Error(), "failed to create chat directory") {
			t.Fatal(err)
		}
	})
	t.Run("incomplete info", func(t *testing.T) {
		for name, tc := range map[string]struct {
			url    string
			length uint64
		}{"no url": {"", 9}, "no length": {url, 0}} {
			t.Run(name, func(t *testing.T) {
				s := newTestStore(t)
				seedTyped(t, s, "m", "image", "a.jpg", tc.url, tc.length)
				fc := &fakeClient{}
				if err := attempt(t, fc, s, "m"); !strings.Contains(err.Error(), "incomplete media information") || fc.downloads != 0 {
					t.Fatalf("err=%v downloads=%d", err, fc.downloads)
				}
			})
		}
	})
	t.Run("unsupported media type", func(t *testing.T) {
		s := newTestStore(t)
		seedTyped(t, s, "m", "sticker", "a.webp", url, 9)
		fc := &fakeClient{}
		if err := attempt(t, fc, s, "m"); !strings.Contains(err.Error(), "unsupported media type: sticker") || fc.downloads != 0 {
			t.Fatalf("err=%v downloads=%d", err, fc.downloads)
		}
	})
	t.Run("download fails", func(t *testing.T) {
		root := t.TempDir()
		s := newTestStoreIn(t, root)
		seedTyped(t, s, "m", "image", "a.jpg", url, 9)
		fc := &fakeClient{downloadErr: errors.New("cdn 404")}
		if err := attempt(t, fc, s, "m"); err.Error() != "failed to download media: cdn 404" {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, sanitizeChatDirName(dlChat), "a.jpg")); !os.IsNotExist(err) {
			t.Fatalf("no file may be left behind: %v", err)
		}
	})
	t.Run("file cannot be saved", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		root := t.TempDir()
		s := newTestStoreIn(t, root)
		seedTyped(t, s, "m", "image", "a.jpg", url, 9)
		chatDir := filepath.Join(root, sanitizeChatDirName(dlChat))
		if err := os.Mkdir(chatDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(chatDir, 0o700) })
		fc := &fakeClient{data: []byte("x")}
		if err := attempt(t, fc, s, "m"); !strings.Contains(err.Error(), "failed to save media file") {
			t.Fatal(err)
		}
	})
}

func TestSafeMediaPathDefensiveErrors(t *testing.T) {
	root := t.TempDir()
	if _, _, _, err := safeMediaPath(root, "..", "a.jpg"); err == nil || !strings.Contains(err.Error(), "invalid chat JID") {
		t.Fatalf("dots-only chat: %v", err)
	}
	if _, _, _, err := safeMediaPath(root, "chat", "/"); err == nil {
		t.Fatal("separator-only filename accepted")
	}

	oldAbs, oldRel := filepathAbs, filepathRel
	t.Cleanup(func() { filepathAbs, filepathRel = oldAbs, oldRel })

	filepathAbs = func(string) (string, error) { return "", errors.New("no cwd") }
	if _, _, _, err := safeMediaPath(root, "chat", "a.jpg"); err == nil || !strings.Contains(err.Error(), "failed to resolve store directory") {
		t.Fatalf("abs failure: %v", err)
	}
	filepathAbs = oldAbs

	for name, rel := range map[string]func(string, string) (string, error){
		"error":    func(string, string) (string, error) { return "", errors.New("rel boom") },
		"parent":   func(string, string) (string, error) { return "..", nil },
		"up":       func(string, string) (string, error) { return "../x", nil },
		"absolute": func(string, string) (string, error) { return "/etc/passwd", nil },
	} {
		filepathRel = rel
		if _, _, _, err := safeMediaPath(root, "chat", "a.jpg"); err == nil || !strings.Contains(err.Error(), "escapes store directory") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

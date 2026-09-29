package frigate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oldtyt/frigate-telegram/internal/config"
	"github.com/oldtyt/frigate-telegram/internal/log"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestMain(m *testing.M) {
	log.LogFunc()
	os.Exit(m.Run())
}

func TestEventReadyToSend(t *testing.T) {
	t.Setenv("TIME_WAIT_SAVE", "30")
	conf := config.New()
	now := float64(time.Now().Unix())

	tests := []struct {
		name  string
		event EventStruct
		want  bool
	}{
		{"in progress", EventStruct{HasClip: true, EndTime: 0}, false},
		{"just ended with clip", EventStruct{HasClip: true, EndTime: now - 5}, false},
		{"ended long enough ago", EventStruct{HasClip: true, EndTime: now - 60}, true},
		{"just ended without clip", EventStruct{HasClip: false, EndTime: now - 5}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := eventReadyToSend(tt.event, conf); got != tt.want {
				t.Errorf("eventReadyToSend() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDownloadMedia(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			// Streamed without Content-Length, like Frigate's clip.mp4.
			for i := 0; i < 3; i++ {
				_, _ = w.Write([]byte("0123456789"))
				w.(http.Flusher).Flush()
			}
		case "/empty":
		case "/missing":
			http.Error(w, "No recordings found", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	conf := config.New()
	prefix := fmt.Sprintf("/tmp/frigate-telegram-test-%d-", time.Now().UnixNano())
	pattern := strings.TrimPrefix(prefix, "/tmp/") + "*.mp4"

	path, err := downloadMedia(context.Background(), conf, server.URL+"/ok", pattern, 1000)
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) != 30 {
		t.Errorf("got %d bytes (err %v), want 30", len(data), err)
	}
	removeFile(path)

	if _, err := downloadMedia(context.Background(), conf, server.URL+"/ok", pattern, 29); !errors.Is(err, errMediaTooLarge) {
		t.Errorf("oversized stream: got %v, want errMediaTooLarge", err)
	}
	if _, err := downloadMedia(context.Background(), conf, server.URL+"/empty", pattern, 1000); err == nil {
		t.Error("empty response: expected an error")
	}
	if _, err := downloadMedia(context.Background(), conf, server.URL+"/missing", pattern, 1000); err == nil {
		t.Error("bad status: expected an error")
	}

	if leftovers, _ := filepath.Glob(prefix + "*"); len(leftovers) != 0 {
		t.Errorf("failed downloads left files behind: %v", leftovers)
	}
}

// fakeTelegram records sendMediaGroup requests. The first failFirst requests
// are rejected with flood control.
type fakeTelegram struct {
	mu        sync.Mutex
	failFirst int
	calls     int
	sent      [][]map[string]any
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot","username":"bot"}}`)
	case strings.HasSuffix(r.URL.Path, "/sendMediaGroup"):
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if f.calls <= f.failFirst {
			_, _ = fmt.Fprint(w, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`)
			return
		}
		var media []map[string]any
		_ = json.Unmarshal([]byte(r.FormValue("media")), &media)
		f.sent = append(f.sent, media)
		_, _ = fmt.Fprint(w, `{"ok":true,"result":[{"message_id":1,"date":0,"chat":{"id":1,"type":"private"}}]}`)
	default:
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":1,"type":"private"}}}`)
	}
}

// fakeFrigate serves a thumbnail and a clip that fails clipFailures times
// before it becomes available, like a slow Frigate still saving recordings.
func fakeFrigate(t *testing.T, eventID string, clipFailures int) *httptest.Server {
	var mu sync.Mutex
	clipRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/events/" + eventID + "/thumbnail.jpg":
			_, _ = w.Write([]byte("jpeg"))
		case "/api/events/" + eventID + "/clip.mp4":
			mu.Lock()
			clipRequests++
			n := clipRequests
			mu.Unlock()
			if n <= clipFailures {
				http.Error(w, `{"success":false,"message":"No recordings found for the specified time range"}`, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte("mp4 data"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func setupSend(t *testing.T, frigateURL string, telegram *fakeTelegram) *tgbotapi.BotAPI {
	t.Setenv("FRIGATE_URL", frigateURL)
	t.Setenv("TELEGRAM_CHAT_ID", "1")
	oldDelay := clipRetryDelay
	clipRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { clipRetryDelay = oldDelay })

	server := httptest.NewServer(telegram)
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("token", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return bot
}

func testEvent(id string) EventStruct {
	now := float64(time.Now().Unix())
	return EventStruct{ID: id, Camera: "front", Label: "person", StartTime: now - 120, EndTime: now - 60, HasClip: true}
}

func TestSendMessageEventRetriesClipAndTelegram(t *testing.T) {
	event := testEvent("1700000000.000001-retry")
	telegram := &fakeTelegram{failFirst: 1}
	bot := setupSend(t, fakeFrigate(t, event.ID, 2).URL, telegram)
	t.Setenv("CLIP_RETRY_TIMEOUT", "30")

	SendMessageEvent(context.Background(), event, bot)

	if telegram.calls != 2 || len(telegram.sent) != 1 {
		t.Fatalf("got %d sendMediaGroup calls and %d sent groups, want 2 and 1", telegram.calls, len(telegram.sent))
	}
	media := telegram.sent[0]
	if len(media) != 2 || media[0]["type"] != "photo" || media[1]["type"] != "video" {
		t.Fatalf("want photo + video, got %v", media)
	}
	if caption, _ := media[0]["caption"].(string); strings.Contains(caption, "not attached") {
		t.Errorf("caption should not report a missing clip: %q", caption)
	}
	if leftovers, _ := filepath.Glob("/tmp/" + event.ID + "*"); len(leftovers) != 0 {
		t.Errorf("temporary files not removed: %v", leftovers)
	}
}

func TestSendMessageEventExplainsMissingClip(t *testing.T) {
	event := testEvent("1700000000.000002-noclip")
	telegram := &fakeTelegram{}
	bot := setupSend(t, fakeFrigate(t, event.ID, 1000).URL, telegram)
	t.Setenv("CLIP_RETRY_TIMEOUT", "0")

	SendMessageEvent(context.Background(), event, bot)

	if len(telegram.sent) != 1 || len(telegram.sent[0]) != 1 {
		t.Fatalf("want one group with only the thumbnail, got %v", telegram.sent)
	}
	caption, _ := telegram.sent[0][0]["caption"].(string)
	if !strings.Contains(caption, "Clip not attached: not available from Frigate after 1 attempts") {
		t.Errorf("caption should explain the missing clip: %q", caption)
	}
}

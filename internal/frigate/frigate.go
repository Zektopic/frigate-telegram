package frigate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oldtyt/frigate-telegram/internal/config"
	"github.com/oldtyt/frigate-telegram/internal/log"
	"github.com/oldtyt/frigate-telegram/internal/redis"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type EventsStruct []struct {
	Box    interface{} `json:"box"`
	Camera string      `json:"camera"`
	Data   struct {
		Attributes []interface{} `json:"attributes"`
		Box        []float64     `json:"box"`
		Region     []float64     `json:"region"`
		Score      float64       `json:"score"`
		TopScore   float64       `json:"top_score"`
		Type       string        `json:"type"`
	} `json:"data"`
	EndTime            float64     `json:"end_time"`
	FalsePositive      interface{} `json:"false_positive"`
	HasClip            bool        `json:"has_clip"`
	HasSnapshot        bool        `json:"has_snapshot"`
	ID                 string      `json:"id"`
	Label              string      `json:"label"`
	PlusID             interface{} `json:"plus_id"`
	RetainIndefinitely bool        `json:"retain_indefinitely"`
	StartTime          float64     `json:"start_time"`
	SubLabel           interface{} `json:"sub_label"`
	Thumbnail          string      `json:"thumbnail"`
	TopScore           interface{} `json:"top_score"`
	Zones              []any       `json:"zones"`
}

type EventStruct struct {
	Box    interface{} `json:"box"`
	Camera string      `json:"camera"`
	Data   struct {
		Attributes []interface{} `json:"attributes"`
		Box        []float64     `json:"box"`
		Region     []float64     `json:"region"`
		Score      float64       `json:"score"`
		TopScore   float64       `json:"top_score"`
		Type       string        `json:"type"`
	} `json:"data"`
	EndTime            float64     `json:"end_time"`
	FalsePositive      interface{} `json:"false_positive"`
	HasClip            bool        `json:"has_clip"`
	HasSnapshot        bool        `json:"has_snapshot"`
	ID                 string      `json:"id"`
	Label              string      `json:"label"`
	PlusID             interface{} `json:"plus_id"`
	RetainIndefinitely bool        `json:"retain_indefinitely"`
	StartTime          float64     `json:"start_time"`
	SubLabel           interface{} `json:"sub_label"`
	Thumbnail          string      `json:"thumbnail"`
	TopScore           interface{} `json:"top_score"`
	Zones              []any       `json:"zones"`
}

// telegramMaxUploadSize is the largest file a bot can upload.
// See https://github.com/OldTyT/frigate-telegram/issues/5
const telegramMaxUploadSize = 50 * 1024 * 1024

// telegramMaxPhotoSize is the largest photo a bot can send.
const telegramMaxPhotoSize = 10 * 1024 * 1024

// maxEventsResponseSize caps how much of an /api/events response is read, so a
// misbehaving server can't exhaust memory.
const maxEventsResponseSize = 32 * 1024 * 1024

// telegramSendAttempts is how many times a message is sent before giving up.
const telegramSendAttempts = 3

// clipRetryDelay is the wait before the first clip download retry. It doubles
// after every failed attempt, up to 2 minutes.
var clipRetryDelay = 15 * time.Second

// eventQueue holds finished events waiting to be sent. A fixed pool of
// EVENT_WORKERS workers drains it, so slow machines aren't overloaded by
// parallel clip downloads and uploads: events wait in line instead of
// losing their clip.
var eventQueue = make(chan EventStruct, 100)

// queuedEvents holds the IDs of events that are queued or being sent, so a
// slow send is never picked up a second time by a later poll.
var (
	queuedEvents   = make(map[string]struct{})
	queuedEventsMu sync.Mutex
)

// errMediaTooLarge means a file is over the Telegram upload limit.
var errMediaTooLarge = errors.New("file is larger than the Telegram upload limit")

// httpClient is a shared HTTP client with a reasonable timeout to prevent
// goroutine leaks when the Frigate server is unreachable or hangs.
// Clips and previews use downloadMedia instead, which has a longer timeout.
var httpClient = &http.Client{
	Timeout: 60 * time.Second,
}

// errorThrottle tracks the last time each error type was sent to Telegram
// to avoid flooding the chat with repeated error messages.
var (
	lastErrorSend   time.Time
	lastWarnSend    time.Time
	errorThrottleMu sync.Mutex
)

const errorThrottleInterval = 15 * time.Minute

// throttleErrorSend returns true if the error should be sent (enough time has passed).
func throttleErrorSend() bool {
	errorThrottleMu.Lock()
	defer errorThrottleMu.Unlock()
	if time.Since(lastErrorSend) < errorThrottleInterval {
		return false
	}
	lastErrorSend = time.Now()
	return true
}

// throttleWarnSend returns true if the warning should be sent (enough time has passed).
func throttleWarnSend() bool {
	errorThrottleMu.Lock()
	defer errorThrottleMu.Unlock()
	if time.Since(lastWarnSend) < errorThrottleInterval {
		return false
	}
	lastWarnSend = time.Now()
	return true
}

// NormalizeTagText keeps only the ASCII letters and digits of text, so it can
// be used as a Telegram hashtag. It runs several times per event, so it works
// on bytes: every byte of a multi-byte UTF-8 rune is >= 0x80 and is dropped.
func NormalizeTagText(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); i++ {
		c := text[i]
		if ('0' <= c && c <= '9') || ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func GetTagList(subLabel interface{}) []string {
	var my_tags []string
	switch v := subLabel.(type) {
	case string:
		if v != "" {
			my_tags = append(my_tags, NormalizeTagText(v))
		}
	case []interface{}:
		for _, item := range v {
			switch itemVal := item.(type) {
			case string:
				my_tags = append(my_tags, NormalizeTagText(itemVal))
			case float64:
				// Maybe: my_tags = append(my_tags, fmt.Sprintf("%.2f%%", itemVal*100))
			}
		}
	case nil:
	default:
		log.Warn.Printf("Unexpected sub_label type: %T", v)
	}
	return my_tags
}

func ErrorSend(TextError string, bot *tgbotapi.BotAPI, EventID string) {
	log.Error.Println(TextError + "\nEventID: " + EventID)
	// Throttle Telegram error notifications to avoid flooding the chat
	if !throttleErrorSend() {
		return
	}
	conf := config.New()
	_, err := bot.Send(tgbotapi.NewMessage(conf.TelegramChatID, TextError+"\nEventID: "+EventID))
	if err != nil {
		log.Error.Println(err.Error())
	}
}

func WarnSend(TextError string, bot *tgbotapi.BotAPI, EventID string) {
	log.Warn.Println(TextError + "\nEventID: " + EventID)
	// Throttle Telegram warning notifications to avoid flooding the chat
	if !throttleWarnSend() {
		return
	}
	conf := config.New()
	_, err := bot.Send(tgbotapi.NewMessage(conf.TelegramChatID, TextError+"\nEventID: "+EventID))
	if err != nil {
		log.Error.Println(err.Error())
	}
}

// SaveThumbnail writes the base64 thumbnail from the event data to a new file
// in /tmp and returns its path, or "" on failure.
func SaveThumbnail(EventID string, Thumbnail string, bot *tgbotapi.BotAPI) string {
	log.Debug.Printf("Processing thumbnail for event ID: %s", EventID)

	// Verify that we have a non-empty thumbnail string
	if Thumbnail == "" {
		ErrorSend("Empty thumbnail string received", bot, EventID)
		return ""
	}

	// Decode string Thumbnail base64
	dec, err := base64.StdEncoding.DecodeString(Thumbnail)
	if err != nil {
		ErrorSend("Error when base64 string decode: "+err.Error(), bot, EventID)
		return ""
	}
	if len(dec) == 0 {
		ErrorSend("Decoded thumbnail is empty", bot, EventID)
		return ""
	}

	// A unique file, like downloadMedia: the event ID comes from Frigate's API
	// and isn't used as a path, and os.CreateTemp rejects IDs containing "/".
	f, err := os.CreateTemp("/tmp", EventID+"-*.jpg")
	if err != nil {
		ErrorSend("Error when create file: "+err.Error(), bot, EventID)
		return ""
	}
	_, err = f.Write(dec)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		removeFile(f.Name())
		ErrorSend("Error when write file: "+err.Error(), bot, EventID)
		return ""
	}

	log.Debug.Printf("Saved %d byte thumbnail to %s", len(dec), f.Name())
	return f.Name()
}

// DownloadThumbnail downloads the event thumbnail to a new file in /tmp and
// returns its path, or "" on failure.
func DownloadThumbnail(ctx context.Context, EventID string, bot *tgbotapi.BotAPI) string {
	// Get config
	conf := config.New()

	ThumbnailURL := eventFileURL(conf.FrigateURL, EventID, "thumbnail.jpg")
	log.Debug.Println("Downloading thumbnail from URL: " + ThumbnailURL)

	filename, err := downloadMedia(ctx, conf, ThumbnailURL, EventID+"-*.jpg", telegramMaxPhotoSize)
	if err != nil {
		if ctx.Err() == nil {
			ErrorSend("Error thumbnail download: "+err.Error(), bot, EventID)
		}
		return ""
	}
	return filename
}

// eventFileURL returns the Frigate API URL of one of an event's files. The
// event ID comes from Frigate's API response, so it's escaped, not trusted.
func eventFileURL(baseURL string, EventID string, file string) string {
	return baseURL + "/api/events/" + url.PathEscape(EventID) + "/" + file
}

// GetEvents fetches the latest events from Frigate. It returns no events on
// error, or when ctx is cancelled.
func GetEvents(ctx context.Context, FrigateURL string, bot *tgbotapi.BotAPI, SetBefore bool) EventsStruct {
	conf := config.New()

	// Thumbnails are base64 images that make up most of the response, and
	// they're polled every few seconds. Only events that are sent need one, and
	// saveEventThumbnail downloads it then.
	FrigateURL = FrigateURL + "?limit=" + strconv.Itoa(conf.FrigateEventLimit) + "&include_thumbnails=0"

	if SetBefore {
		timestamp := time.Now().UTC().Unix()
		timestamp = timestamp - int64(conf.EventBeforeSeconds)
		FrigateURL = FrigateURL + "&before=" + strconv.FormatInt(timestamp, 10)
	}

	log.Debug.Println("Getting events from Frigate via URL: " + FrigateURL)

	// Request to Frigate
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, FrigateURL, nil)
	if err != nil {
		ErrorSend("Error get events from Frigate, error: "+err.Error(), bot, "ALL")
		return EventsStruct{}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			ErrorSend("Error get events from Frigate, error: "+err.Error(), bot, "ALL")
		}
		return EventsStruct{}
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Error.Println("Error closing response body: " + err.Error())
		}
	}()

	// Check response status code
	if resp.StatusCode != 200 {
		WarnSend("Response status != 200, when getting events from Frigate.", bot, "ALL")
		return EventsStruct{}
	}

	// Read data from response
	byteValue, err := io.ReadAll(io.LimitReader(resp.Body, maxEventsResponseSize+1))
	if err != nil {
		if ctx.Err() == nil {
			ErrorSend("Can't read JSON: "+err.Error(), bot, "ALL")
		}
		return EventsStruct{}
	}
	if len(byteValue) > maxEventsResponseSize {
		ErrorSend(fmt.Sprintf("Events response from Frigate is larger than %d MB, ignoring it.", maxEventsResponseSize/1024/1024), bot, "ALL")
		return EventsStruct{}
	}

	// Parse data from JSON to a local struct (not package-level to avoid data race)
	var events EventsStruct
	if err := json.Unmarshal(byteValue, &events); err != nil {
		ErrorSend("Error unmarshal json: "+err.Error(), bot, "ALL")
		if e, ok := err.(*json.SyntaxError); ok {
			log.Info.Println("syntax error at byte offset " + strconv.Itoa(int(e.Offset)))
		}
		log.Info.Println("Exit.")
		return EventsStruct{}
	}

	// Return events
	return events
}

// SaveClip downloads the event clip to /tmp. Frigate builds clips from
// recording segments on request, which is slow on weaker hardware and fails
// until the segments are saved, so failed or empty downloads are retried with
// backoff for up to CLIP_RETRY_TIMEOUT seconds. On failure it returns "" and a
// short reason to show in the message caption.
func SaveClip(ctx context.Context, EventID string) (string, string) {
	// Get config
	conf := config.New()

	// Generate clip URL
	ClipURL := eventFileURL(conf.FrigateURL, EventID, "clip.mp4")

	deadline := time.Now().Add(time.Duration(conf.ClipRetryTimeout) * time.Second)
	delay := clipRetryDelay
	for attempt := 1; ; attempt++ {
		log.Debug.Printf("Downloading clip from URL: %s (attempt %d)", ClipURL, attempt)
		filename, err := downloadMedia(ctx, conf, ClipURL, EventID+"-*.mp4", telegramMaxUploadSize)
		if err == nil {
			return filename, ""
		}
		if errors.Is(err, errMediaTooLarge) {
			return "", "larger than the 50 MB Telegram limit"
		}
		if ctx.Err() != nil {
			return "", "shutting down"
		}
		log.Warn.Printf("Clip download for event %s failed (attempt %d): %s", EventID, attempt, err.Error())

		if time.Now().Add(delay).After(deadline) {
			return "", fmt.Sprintf("not available from Frigate after %d attempts", attempt)
		}
		if !sleepContext(ctx, delay) {
			return "", "shutting down"
		}
		delay = min(delay*2, 2*time.Minute)
	}
}

// SavePreview downloads the event preview to /tmp. Previews are optional, so
// failures are only logged and "" is returned.
func SavePreview(ctx context.Context, EventID string) string {
	// Get config
	conf := config.New()

	// Generate preview URL
	PreviewURL := eventFileURL(conf.FrigateURL, EventID, "preview.mp4")
	log.Debug.Println("Downloading preview from URL: " + PreviewURL)

	filename, err := downloadMedia(ctx, conf, PreviewURL, EventID+"-*_preview.mp4", telegramMaxUploadSize)
	if err != nil {
		// Preview might not be available in older Frigate versions or if not generated yet
		log.Debug.Printf("Preview not available, skipping: %s", err.Error())
		return ""
	}
	return filename
}

// downloadMedia downloads url into a new file in /tmp named after pattern
// (see os.CreateTemp) and returns its path. A unique file per download means
// two sends can never overwrite or delete each other's files. Downloads that
// fail, are empty, or are larger than maxSize leave no file behind.
func downloadMedia(ctx context.Context, conf *config.Config, url string, pattern string, maxSize int64) (string, error) {
	// Frigate streams clips while ffmpeg assembles them, so the whole transfer
	// can take minutes on slow hardware; the 60s API timeout is too short.
	client := &http.Client{Timeout: time.Duration(conf.MediaDownloadTimeout) * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Error.Println("Error closing response body: " + err.Error())
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bad status: %s", resp.Status)
	}
	if resp.ContentLength > maxSize {
		return "", errMediaTooLarge
	}

	f, err := os.CreateTemp("/tmp", pattern)
	if err != nil {
		return "", err
	}

	// Read one byte past maxSize to detect oversized streams without
	// buffering all of them to disk.
	bytesWritten, err := io.Copy(f, io.LimitReader(resp.Body, maxSize+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && bytesWritten == 0 {
		err = errors.New("empty response")
	}
	if err == nil && bytesWritten > maxSize {
		err = errMediaTooLarge
	}
	if err != nil {
		removeFile(f.Name())
		return "", err
	}

	log.Debug.Printf("Downloaded %d bytes to %s", bytesWritten, f.Name())
	return f.Name(), nil
}

// sleepContext waits for d and returns true, or returns false early if ctx is
// cancelled.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func removeFile(path string) {
	if err := os.Remove(path); err != nil {
		log.Debug.Println("Error removing file: " + err.Error())
	}
}

// SendMessageEvent sends an event to Telegram with its thumbnail, clip and
// preview attached. If the clip can't be attached, the caption says why.
func SendMessageEvent(ctx context.Context, FrigateEvent EventStruct, bot *tgbotapi.BotAPI) {
	// Get config
	conf := config.New()

	// Prepare text message
	text := ""
	t_start := time.Unix(int64(FrigateEvent.StartTime), 0)
	if conf.ShortEventMessageFormat {
		// Short message format
		text += fmt.Sprintf("#%s detected on #%s at %s",
			NormalizeTagText(FrigateEvent.Label),
			NormalizeTagText(FrigateEvent.Camera),
			t_start)

	} else {
		// Normal message format
		text += "*Event*\n"
		text += "┣*Camera*\n┗ #" + NormalizeTagText(FrigateEvent.Camera) + "\n"
		text += "┣*Label*\n┗ #" + NormalizeTagText(FrigateEvent.Label) + "\n"
		SubLabels := GetTagList(FrigateEvent.SubLabel)
		if len(SubLabels) > 0 {
			if FrigateEvent.SubLabel != nil {
				text += "┣*SubLabel*\n┗ #" + strings.Join(SubLabels, ", #") + "\n"
			}
		}
		text += fmt.Sprintf("┣*Start time*\n┗ `%s", t_start) + "`\n"
		if FrigateEvent.EndTime == 0 {
			text += "┣*End time*\n┗ `In progress`" + "\n"
		} else {
			t_end := time.Unix(int64(FrigateEvent.EndTime), 0)
			text += fmt.Sprintf("┣*End time*\n┗ `%s", t_end) + "`\n"
		}
		text += fmt.Sprintf("┣*Top score*\n┗ `%f", (FrigateEvent.Data.TopScore*100)) + "%`\n"
		text += "┣*Event id*\n┗ `" + FrigateEvent.ID + "`\n"
		text += "┣*Zones*\n┗ #" + strings.Join(GetTagList(FrigateEvent.Zones), ", #") + "\n"
		text += "*URLs*\n"
		text += "┣[Events](" + conf.FrigateExternalURL + "/events?cameras=" + FrigateEvent.Camera + "&labels=" + FrigateEvent.Label + "&zones=" + strings.Join(GetTagList(FrigateEvent.Zones), ",") + ")\n"
		text += "┣[General](" + conf.FrigateExternalURL + ")\n"
		text += "┗[Source clip](" + eventFileURL(conf.FrigateExternalURL, FrigateEvent.ID, "clip.mp4") + ")\n"
	}

	var FilePathThumbnail string
	if conf.IncludeThumbnailEvent {
		FilePathThumbnail = saveEventThumbnail(ctx, FrigateEvent, bot)
		if FilePathThumbnail != "" {
			defer removeFile(FilePathThumbnail)
		}
	}

	var videos []string
	clipNote := ""
	if conf.IncludeClipEvent && FrigateEvent.HasClip {
		FilePathClip, reason := SaveClip(ctx, FrigateEvent.ID)
		if ctx.Err() != nil {
			// Shutting down: leave the event unmarked so it's sent after restart.
			return
		}
		if FilePathClip != "" {
			defer removeFile(FilePathClip)
			videos = append(videos, FilePathClip)
		} else {
			log.Warn.Printf("Sending event %s without clip: %s", FrigateEvent.ID, reason)
			clipNote = "\n⚠️ Clip not attached: " + reason
		}
	}

	if conf.IncludePreviewEvent {
		if FilePathPreview := SavePreview(ctx, FrigateEvent.ID); FilePathPreview != "" {
			defer removeFile(FilePathPreview)
			videos = append(videos, FilePathPreview)
		}
	}

	log.Debug.Printf("Sending event %s with %d video(s)", FrigateEvent.ID, len(videos))
	err := sendEventMessage(ctx, bot, conf, text+clipNote, FilePathThumbnail, videos)
	if err != nil && len(videos) > 0 && ctx.Err() == nil {
		// Still deliver the event if Telegram keeps rejecting the video upload.
		log.Warn.Printf("Retrying event %s without video: %s", FrigateEvent.ID, err.Error())
		if clipNote == "" {
			clipNote = "\n⚠️ Video not attached: upload to Telegram failed"
		}
		err = sendEventMessage(ctx, bot, conf, text+clipNote, FilePathThumbnail, nil)
	}
	if err != nil {
		if ctx.Err() != nil {
			// Shutting down: leave the event unmarked so it's sent after restart.
			return
		}
		ErrorSend("Error sending event to Telegram: "+err.Error(), bot, FrigateEvent.ID)
	}

	redis.AddNewEvent(FrigateEvent.ID, "Finished", time.Duration(conf.RedisTTL)*time.Second)
}

// saveEventThumbnail saves the event thumbnail to /tmp, from the base64 data
// in the event if possible, otherwise by downloading it. Returns "" on failure.
// Both ways only return files that were fully written and aren't empty.
func saveEventThumbnail(ctx context.Context, FrigateEvent EventStruct, bot *tgbotapi.BotAPI) string {
	if FrigateEvent.Thumbnail != "" {
		log.Debug.Println("Using base64 thumbnail from event data")
		if FilePathThumbnail := SaveThumbnail(FrigateEvent.ID, FrigateEvent.Thumbnail, bot); FilePathThumbnail != "" {
			return FilePathThumbnail
		}
		log.Debug.Println("Base64 thumbnail failed, trying direct download")
	} else {
		log.Debug.Println("No thumbnail in event data, downloading directly")
	}
	return DownloadThumbnail(ctx, FrigateEvent.ID, bot)
}

// sendEventMessage sends the thumbnail and videos as one media group with
// the caption on the first item, or a text message if there is no media.
func sendEventMessage(ctx context.Context, bot *tgbotapi.BotAPI, conf *config.Config, text string, thumbnail string, videos []string) error {
	var medias []interface{}
	if thumbnail != "" {
		MediaThumbnail := tgbotapi.NewInputMediaPhoto(tgbotapi.FilePath(thumbnail))
		MediaThumbnail.Caption = text
		MediaThumbnail.ParseMode = tgbotapi.ModeMarkdown
		medias = append(medias, MediaThumbnail)
	}
	for _, video := range videos {
		MediaVideo := tgbotapi.NewInputMediaVideo(tgbotapi.FilePath(video))
		if len(medias) == 0 {
			MediaVideo.Caption = text
			MediaVideo.ParseMode = tgbotapi.ModeMarkdown
		}
		medias = append(medias, MediaVideo)
	}

	if len(medias) == 0 {
		msg := tgbotapi.NewMessage(conf.TelegramChatID, text)
		msg.ParseMode = tgbotapi.ModeMarkdown
		msg.DisableNotification = redis.GetStateMuteEvent()
		return sendWithRetry(ctx, func() error {
			_, err := bot.Send(msg)
			return err
		})
	}

	msg := tgbotapi.MediaGroupConfig{
		ChatID: conf.TelegramChatID,
		Media:  medias,
	}
	msg.DisableNotification = redis.GetStateMuteEvent()
	return sendWithRetry(ctx, func() error {
		_, err := bot.SendMediaGroup(msg)
		return err
	})
}

// sendWithRetry calls send until it succeeds or telegramSendAttempts is
// reached, backing off between attempts and honouring Telegram flood control.
func sendWithRetry(ctx context.Context, send func() error) error {
	delay := 5 * time.Second
	for attempt := 1; ; attempt++ {
		err := send()
		if err == nil {
			return nil
		}
		log.Warn.Printf("Telegram send failed (attempt %d/%d): %s", attempt, telegramSendAttempts, err.Error())
		if attempt >= telegramSendAttempts {
			return err
		}

		wait := delay
		var tgErr *tgbotapi.Error
		if errors.As(err, &tgErr) && tgErr.RetryAfter > 0 {
			wait = time.Duration(tgErr.RetryAfter) * time.Second
		}
		if !sleepContext(ctx, wait) {
			return err
		}
		delay *= 2
	}
}

func StringsContains(MyStr string, MySlice []string) bool {
	for _, v := range MySlice {
		if v == MyStr {
			return true
		}
	}
	return false
}

func ParseEvents(FrigateEvents EventsStruct, bot *tgbotapi.BotAPI, WatchDog bool) {
	// Parse events
	conf := config.New()
	RedisKeyPrefix := ""
	if WatchDog {
		RedisKeyPrefix = "WatchDog_"
	}
	for Event := range FrigateEvents {
		// Skip by camera
		if len(conf.FrigateExcludeCamera) != 1 || conf.FrigateExcludeCamera[0] != "None" {
			if StringsContains(FrigateEvents[Event].Camera, conf.FrigateExcludeCamera) {
				log.Debug.Println("Skipping event from exclude camera: " + FrigateEvents[Event].Camera)
				continue
			}
		}
		if len(conf.FrigateIncludeCamera) != 1 || conf.FrigateIncludeCamera[0] != "All" {
			if !(StringsContains(FrigateEvents[Event].Camera, conf.FrigateIncludeCamera)) {
				log.Debug.Println("Skipping event from include camera: " + FrigateEvents[Event].Camera)
				continue
			}
		}
		// End skip by camera

		// Skip by label
		if len(conf.FrigateExcludeLabel) != 1 || conf.FrigateExcludeLabel[0] != "None" {
			if StringsContains(FrigateEvents[Event].Label, conf.FrigateExcludeLabel) {
				log.Debug.Println("Skipping event by exclude label: " + FrigateEvents[Event].Label)
				continue
			}
		}
		if len(conf.FrigateIncludeLabel) != 1 || conf.FrigateIncludeLabel[0] != "All" {
			if !(StringsContains(FrigateEvents[Event].Label, conf.FrigateIncludeLabel)) {
				log.Debug.Println("Skipping event by include label: " + FrigateEvents[Event].Label)
				continue
			}
		}
		// Skip by label

		// Skip by zone
		zones := GetTagList(FrigateEvents[Event].Zones)
		needSkip := false
		if len(conf.FrigateExcludeZone) != 1 || conf.FrigateExcludeZone[0] != "None" {
			if len(zones) != 0 {
				for _, zone := range zones {
					if StringsContains(zone, conf.FrigateExcludeZone) {
						log.Debug.Println("Skipping event by exclude zone: " + zone)
						needSkip = true
					}
				}
			}
		}
		if needSkip {
			continue
		}
		if len(conf.FrigateIncludeZone) != 1 || conf.FrigateIncludeZone[0] != "All" {
			if len(zones) == 0 {
				log.Debug.Println("Skipping the event due to zero zones.")
				continue
			}
			for _, zone := range zones {
				if !(StringsContains(zone, conf.FrigateIncludeZone)) {
					log.Debug.Println("Skipping event by include zone: " + zone)
					needSkip = true
				}
			}
		}
		if needSkip {
			continue
		}
		// Skip by zone

		if !WatchDog {
			if !eventReadyToSend(FrigateEvents[Event], conf) {
				log.Debug.Println("Event not finished yet, will send it later: " + FrigateEvents[Event].ID)
				continue
			}
			if isEventQueued(FrigateEvents[Event].ID) {
				continue
			}
		}

		if redis.CheckEvent(RedisKeyPrefix + FrigateEvents[Event].ID) {
			if WatchDog {
				SendTextEvent(FrigateEvents[Event], bot)
			} else {
				enqueueEvent(FrigateEvents[Event])
			}
		}
	}
}

// eventReadyToSend reports whether an event can be sent with all its media.
// Events are only sent once they have ended and, if a clip will be attached,
// TIME_WAIT_SAVE seconds after the end so Frigate can save the recording.
// Events that aren't ready are picked up again by a later poll.
func eventReadyToSend(event EventStruct, conf *config.Config) bool {
	if event.EndTime == 0 {
		return false
	}
	if !conf.IncludeClipEvent || !event.HasClip {
		return true
	}
	endTime := time.Unix(int64(event.EndTime), 0)
	return time.Since(endTime) >= time.Duration(conf.TimeWaitSave)*time.Second
}

func isEventQueued(EventID string) bool {
	queuedEventsMu.Lock()
	defer queuedEventsMu.Unlock()
	_, ok := queuedEvents[EventID]
	return ok
}

// enqueueEvent queues an event for the workers. If the queue is full the event
// is left for the next poll.
func enqueueEvent(event EventStruct) {
	queuedEventsMu.Lock()
	defer queuedEventsMu.Unlock()
	select {
	case eventQueue <- event:
		queuedEvents[event.ID] = struct{}{}
		log.Debug.Printf("Queued event %s (%d waiting)", event.ID, len(eventQueue))
	default:
		log.Warn.Printf("Event queue is full, event %s will be retried on the next poll", event.ID)
	}
}

// StartEventWorkers starts n workers that send queued events one at a time
// until ctx is cancelled.
func StartEventWorkers(ctx context.Context, bot *tgbotapi.BotAPI, n int) {
	for i := 0; i < n; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case event := <-eventQueue:
					SendMessageEvent(ctx, event, bot)
					// Released only after SendMessageEvent has marked the event in
					// Redis, so the next poll can't queue it again.
					queuedEventsMu.Lock()
					delete(queuedEvents, event.ID)
					queuedEventsMu.Unlock()
				}
			}
		}()
	}
}

func SendTextEvent(FrigateEvent EventStruct, bot *tgbotapi.BotAPI) {
	conf := config.New()
	text := "*New event*\n"
	text += "┣*Camera*\n┗ `" + FrigateEvent.Camera + "`\n"
	text += "┣*Label*\n┗ `" + FrigateEvent.Label + "`\n"
	t_start := time.Unix(int64(FrigateEvent.StartTime), 0)
	text += fmt.Sprintf("┣*Start time*\n┗ `%s", t_start) + "`\n"
	text += fmt.Sprintf("┣*Top score*\n┗ `%f", (FrigateEvent.Data.TopScore*100)) + "%`\n"
	text += "┣*Event id*\n┗ `" + FrigateEvent.ID + "`\n"
	text += "┣*Zones*\n┗ `" + strings.Join(GetTagList(FrigateEvent.Zones), ", ") + "`\n"
	text += "┣*Event URL*\n┗ " + conf.FrigateExternalURL + "/events?cameras=" + FrigateEvent.Camera + "&labels=" + FrigateEvent.Label + "&zones=" + strings.Join(GetTagList(FrigateEvent.Zones), ",")
	msg := tgbotapi.NewMessage(conf.TelegramChatID, text)
	msg.ParseMode = tgbotapi.ModeMarkdown
	msg.DisableNotification = redis.GetStateMuteEvent()
	_, err := bot.Send(msg)
	if err != nil {
		log.Error.Println(err.Error())
	}
	redis.AddNewEvent("WatchDog_"+FrigateEvent.ID, "Finished", time.Duration(conf.RedisTTL)*time.Second)
}

func NotifyEvents(bot *tgbotapi.BotAPI, FrigateEventsURL string, ctx context.Context) {
	conf := config.New()
	for {
		FrigateEvents := GetEvents(ctx, FrigateEventsURL, bot, false)
		ParseEvents(FrigateEvents, bot, true)
		if !sleepContext(ctx, time.Duration(conf.WatchDogSleepTime)*time.Second) {
			log.Info.Println("NotifyEvents: shutting down watchdog loop.")
			return
		}
	}
}

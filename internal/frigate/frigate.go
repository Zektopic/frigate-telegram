package frigate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
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

// activeEvents tracks event IDs currently being processed by goroutines to prevent
// duplicate concurrent processing of the same event.
var activeEvents sync.Map

// eventSemaphore limits concurrent event processing to prevent resource exhaustion.
// Buffer size of 5 means at most 5 events are processed concurrently.
var eventSemaphore = make(chan struct{}, 5)

// httpClient is a shared HTTP client with a reasonable timeout to prevent
// goroutine leaks when the Frigate server is unreachable or hangs.
var httpClient = &http.Client{
	Timeout: 60 * time.Second,
}

// videoHttpClient is used for downloading video clips from Frigate.
// It has a 180-second timeout to allow Frigate enough time to remux and stream large clips.
var videoHttpClient = &http.Client{
	Timeout: 180 * time.Second,
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

func NormalizeTagText(text string) string {
	var alphabetCheck = regexp.MustCompile(`^[A-Za-z]+$`)
	var NormalizedText []string
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		wordString := fmt.Sprintf("%c", runes[i])
		if _, err := strconv.Atoi(wordString); err == nil {
			NormalizedText = append(NormalizedText, wordString)
		}
		if alphabetCheck.MatchString(wordString) {
			NormalizedText = append(NormalizedText, wordString)
		}
	}
	return strings.Join(NormalizedText, "")
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

	// Check if we got any data after decoding
	if len(dec) == 0 {
		ErrorSend("Decoded thumbnail is empty", bot, EventID)
		return ""
	}

	log.Debug.Printf("Decoded thumbnail size: %d bytes", len(dec))

	// Generate unique filename
	filename := fmt.Sprintf("/tmp/%s_%d.jpg", EventID, time.Now().UnixNano())
	f, err := os.Create(filename)
	if err != nil {
		ErrorSend("Error when create file: "+err.Error(), bot, EventID)
		return ""
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Error.Println("Error closing thumbnail file: " + err.Error())
		}
	}()

	// Write data to file
	bytesWritten, err := f.Write(dec)
	if err != nil {
		ErrorSend("Error when write file: "+err.Error(), bot, EventID)
		return ""
	}

	// Check if we wrote anything
	if bytesWritten == 0 {
		ErrorSend("No data written to thumbnail file", bot, EventID)
		return ""
	}

	log.Debug.Printf("Written %d bytes to %s", bytesWritten, filename)

	// Ensure file is properly synced to disk
	err = f.Sync()
	if err != nil {
		ErrorSend("Error when sync file: "+err.Error(), bot, EventID)
		return ""
	}

	// Verify file exists and has content
	fileInfo, err := os.Stat(filename)
	if err != nil {
		ErrorSend("Error verifying thumbnail file: "+err.Error(), bot, EventID)
		return ""
	}

	if fileInfo.Size() == 0 {
		ErrorSend("Thumbnail file is empty after write", bot, EventID)
		return ""
	}

	log.Debug.Printf("Successfully saved thumbnail to %s (size: %d bytes)", filename, fileInfo.Size())
	return filename
}

func DownloadThumbnail(EventID string, bot *tgbotapi.BotAPI) string {
	// Get config
	conf := config.New()

	// Generate thumbnail URL
	ThumbnailURL := conf.FrigateURL + "/api/events/" + EventID + "/thumbnail.jpg"
	log.Debug.Println("Downloading thumbnail from URL: " + ThumbnailURL)

	// Generate unique filename
	filename := fmt.Sprintf("/tmp/%s_%d.jpg", EventID, time.Now().UnixNano())

	// Download thumbnail file
	resp, err := httpClient.Get(ThumbnailURL)
	if err != nil {
		ErrorSend("Error thumbnail download: "+err.Error(), bot, EventID)
		return ""
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Error.Println("Error closing response body: " + err.Error())
		}
	}()

	// Check server response
	if resp.StatusCode != http.StatusOK {
		ErrorSend("Return bad status: "+resp.Status, bot, EventID)
		return ""
	}

	log.Debug.Printf("Expected thumbnail content length: %d bytes", resp.ContentLength)

	// Create thumbnail file
	f, err := os.Create(filename)
	if err != nil {
		ErrorSend("Error when create file: "+err.Error(), bot, EventID)
		return ""
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Error.Println("Error closing thumbnail file: " + err.Error())
		}
	}()

	// Write the body to file
	bytesWritten, err := io.Copy(f, resp.Body)
	if err != nil {
		ErrorSend("Error thumbnail write: "+err.Error(), bot, EventID)
		return ""
	}
	log.Debug.Printf("Written %d bytes to %s", bytesWritten, filename)

	// Check if we wrote anything
	if bytesWritten == 0 {
		ErrorSend("No data written to thumbnail file", bot, EventID)
		return ""
	}

	// Ensure file is properly synced to disk
	err = f.Sync()
	if err != nil {
		ErrorSend("Error syncing file to disk: "+err.Error(), bot, EventID)
		return ""
	}

	// Verify file exists and has content
	fileInfo, err := os.Stat(filename)
	if err != nil {
		ErrorSend("Error verifying thumbnail file: "+err.Error(), bot, EventID)
		return ""
	}

	if fileInfo.Size() == 0 {
		ErrorSend("Thumbnail file is empty after download", bot, EventID)
		return ""
	}

	log.Debug.Printf("Successfully downloaded thumbnail to %s (size: %d bytes)", filename, fileInfo.Size())
	return filename
}

func GetEvents(FrigateURL string, bot *tgbotapi.BotAPI, SetBefore bool) EventsStruct {
	conf := config.New()

	FrigateURL = FrigateURL + "?limit=" + strconv.Itoa(conf.FrigateEventLimit)

	if SetBefore {
		timestamp := time.Now().UTC().Unix()
		timestamp = timestamp - int64(conf.EventBeforeSeconds)
		FrigateURL = FrigateURL + "&before=" + strconv.FormatInt(timestamp, 10)
	}

	log.Debug.Println("Getting events from Frigate via URL: " + FrigateURL)

	// Request to Frigate
	resp, err := httpClient.Get(FrigateURL)
	if err != nil {
		ErrorSend("Error get events from Frigate, error: "+err.Error(), bot, "ALL")
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
	byteValue, err := io.ReadAll(resp.Body)
	if err != nil {
		ErrorSend("Can't read JSON: "+err.Error(), bot, "ALL")
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

func SaveClip(EventID string, bot *tgbotapi.BotAPI) string {
	// Get config
	conf := config.New()

	// Generate clip URL
	ClipURL := conf.FrigateURL + "/api/events/" + EventID + "/clip.mp4"
	log.Debug.Println("Downloading clip from URL: " + ClipURL)

	// Generate unique filename
	filename := fmt.Sprintf("/tmp/%s_%d.mp4", EventID, time.Now().UnixNano())

	// Download clip file using videoHttpClient with extended timeout
	resp, err := videoHttpClient.Get(ClipURL)
	if err != nil {
		ErrorSend("Error clip download: "+err.Error(), bot, EventID)
		return ""
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Error.Println("Error closing response body: " + err.Error())
		}
	}()

	// Check server response
	if resp.StatusCode != http.StatusOK {
		ErrorSend("Return bad status: "+resp.Status, bot, EventID)
		return ""
	}

	log.Debug.Printf("Expected content length: %d bytes", resp.ContentLength)

	// Create clip file
	f, err := os.Create(filename)
	if err != nil {
		ErrorSend("Error when create file: "+err.Error(), bot, EventID)
		return ""
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Error.Println("Error closing clip file: " + err.Error())
		}
	}()

	// Write the body to file
	bytesWritten, err := io.Copy(f, resp.Body)
	if err != nil {
		ErrorSend("Error clip write: "+err.Error(), bot, EventID)
		return ""
	}
	log.Debug.Printf("Written %d bytes to %s", bytesWritten, filename)

	// Check if we wrote anything
	if bytesWritten == 0 {
		WarnSend("No data written to clip file", bot, EventID)
		return ""
	}

	// Ensure file is properly synced to disk
	if err = f.Sync(); err != nil {
		ErrorSend("Error syncing file to disk: "+err.Error(), bot, EventID)
		return ""
	}

	// Verify file exists and has content
	fileInfo, err := os.Stat(filename)
	if err != nil {
		ErrorSend("Error verifying clip file: "+err.Error(), bot, EventID)
		return ""
	}

	if fileInfo.Size() == 0 {
		ErrorSend("Clip file is empty after download", bot, EventID)
		return ""
	}

	log.Debug.Printf("Successfully downloaded clip to %s (size: %d bytes)", filename, fileInfo.Size())
	return filename
}

func SavePreview(EventID string, bot *tgbotapi.BotAPI) string {
	// Get config
	conf := config.New()

	// Generate preview URL
	PreviewURL := conf.FrigateURL + "/api/events/" + EventID + "/preview.mp4"
	log.Debug.Println("Downloading preview from URL: " + PreviewURL)

	// Generate unique filename
	filename := fmt.Sprintf("/tmp/%s_%d_preview.mp4", EventID, time.Now().UnixNano())

	// Download preview file
	resp, err := httpClient.Get(PreviewURL)
	if err != nil {
		ErrorSend("Error preview download: "+err.Error(), bot, EventID)
		return ""
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Error.Println("Error closing response body: " + err.Error())
		}
	}()

	// Check server response
	if resp.StatusCode != http.StatusOK {
		// Preview might not be available in older Frigate versions or if not generated yet
		log.Debug.Printf("Preview not available (status %d), skipping", resp.StatusCode)
		return ""
	}

	// Read content length if available
	contentLength := resp.ContentLength
	if contentLength == 0 {
		log.Debug.Println("Received empty preview from server (content length is 0)")
		return ""
	}

	// Create preview file
	f, err := os.Create(filename)
	if err != nil {
		ErrorSend("Error when create file: "+err.Error(), bot, EventID)
		return ""
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Error.Println("Error closing preview file: " + err.Error())
		}
	}()

	// Write the body to file
	bytesWritten, err := io.Copy(f, resp.Body)
	if err != nil {
		ErrorSend("Error preview write: "+err.Error(), bot, EventID)
		return ""
	}

	if bytesWritten == 0 {
		return ""
	}

	if err = f.Sync(); err != nil {
		ErrorSend("Error syncing preview file to disk: "+err.Error(), bot, EventID)
		return ""
	}

	return filename
}

// CompressClip compresses a video clip using ffmpeg to ensure it fits within Telegram's 50MB file size limit.
// It scales down to 720p height (preserving aspect ratio), encodes with H.264 CRF 28 at veryfast preset,
// and moves the MP4 moov atom to the beginning (+faststart) for quick Telegram mobile playback.
func CompressClip(inputPath string, eventID string) (string, error) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}

	outputPath := fmt.Sprintf("/tmp/%s_%d_compressed.mp4", eventID, time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-y",
		"-i", inputPath,
		"-vf", "scale=-2:min(720\\,ih)",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "28",
		"-c:a", "aac",
		"-b:a", "96k",
		"-movflags", "+faststart",
		outputPath,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(outputPath)
		return "", fmt.Errorf("ffmpeg compression failed: %w, output: %s", err, string(output))
	}

	info, err := os.Stat(outputPath)
	if err != nil || info.Size() == 0 {
		_ = os.Remove(outputPath)
		return "", fmt.Errorf("compressed clip is missing or empty")
	}

	return outputPath, nil
}

func SendMessageEvent(FrigateEvent EventStruct, bot *tgbotapi.BotAPI) {
	// Get config
	conf := config.New()

	// Track all temporary files created during this event run so they are guaranteed
	// to be cleaned up on return, regardless of whether sending succeeded or failed.
	var filesToCleanup []string
	defer func() {
		for _, f := range filesToCleanup {
			if f != "" {
				if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
					log.Debug.Println("Error removing temporary file: " + err.Error())
				}
			}
		}
	}()

	// Set InWork state in Redis with a generous TTL (10 minutes) so slow video
	// downloads/uploads or sleeping never cause Redis key expiration during processing.
	inWorkTTL := time.Duration(conf.TimeWaitSave*2+600) * time.Second
	redis.AddNewEvent(FrigateEvent.ID, "InWork", inWorkTTL)

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
		text += "┗[Source clip](" + conf.FrigateExternalURL + "/api/events/" + FrigateEvent.ID + "/clip.mp4)\n"
	}

	var medias []interface{}
	var FilePathThumbnail string

	if conf.IncludeThumbnailEvent {
		// Save thumbnail
		if FrigateEvent.Thumbnail != "" {
			// Try to use the base64 thumbnail first
			log.Debug.Println("Using base64 thumbnail from event data")
			FilePathThumbnail = SaveThumbnail(FrigateEvent.ID, FrigateEvent.Thumbnail, bot)

			// Verify thumbnail file has content
			fileInfo, err := os.Stat(FilePathThumbnail)
			if err != nil || fileInfo.Size() == 0 {
				log.Debug.Println("Base64 thumbnail failed, trying direct download")
				// If base64 method failed, try direct download
				if FilePathThumbnail != "" {
					_ = os.Remove(FilePathThumbnail)
				}
				FilePathThumbnail = DownloadThumbnail(FrigateEvent.ID, bot)
			}
		} else {
			// No thumbnail in event data, download directly
			log.Debug.Println("No thumbnail in event data, downloading directly")
			FilePathThumbnail = DownloadThumbnail(FrigateEvent.ID, bot)
		}

		if FilePathThumbnail != "" {
			filesToCleanup = append(filesToCleanup, FilePathThumbnail)

			// Verify thumbnail file before adding to media group
			thumbnailInfo, err := os.Stat(FilePathThumbnail)
			if err != nil {
				ErrorSend("Error getting thumbnail file info: "+err.Error(), bot, FrigateEvent.ID)
				FilePathThumbnail = ""
			} else if thumbnailInfo.Size() == 0 {
				log.Error.Printf("Thumbnail file is empty: %s", FilePathThumbnail)
				ErrorSend("Cannot send empty thumbnail file", bot, FrigateEvent.ID)
				FilePathThumbnail = ""
			}

			if FilePathThumbnail != "" {
				MediaThumbnail := tgbotapi.NewInputMediaPhoto(tgbotapi.FilePath(FilePathThumbnail))
				MediaThumbnail.Caption = text
				MediaThumbnail.ParseMode = tgbotapi.ModeMarkdown
				medias = append(medias, MediaThumbnail)
			}
		}
	}

	// Define FilePathClip outside the if block to make it available later
	var FilePathClip string
	var hasClip bool

	if conf.IncludeClipEvent && FrigateEvent.HasClip && FrigateEvent.EndTime != 0 {
		// Wait for fully video event created
		log.Debug.Printf("Waiting %d seconds for clip to be ready", conf.TimeWaitSave)
		time.Sleep(time.Duration(conf.TimeWaitSave) * time.Second)

		// Save clip
		FilePathClip = SaveClip(FrigateEvent.ID, bot)
		if FilePathClip != "" {
			filesToCleanup = append(filesToCleanup, FilePathClip)
			hasClip = true

			videoInfo, err := os.Stat(FilePathClip)
			if err != nil {
				ErrorSend("Error receiving information about the clip file: "+err.Error(), bot, FrigateEvent.ID)
				hasClip = false
			} else if videoInfo.Size() == 0 {
				log.Error.Printf("Clip file is empty: %s", FilePathClip)
				hasClip = false
			}

			if hasClip {
				clipSize := videoInfo.Size()
				uploadFilePath := FilePathClip

				// Telegram Bot API limit is 50MB (52428800 bytes).
				// If clip is >= 50MB, compress it using ffmpeg so it can be uploaded.
				if clipSize >= 52428800 {
					log.Info.Printf("Event %s clip size (%.2f MB) exceeds Telegram 50MB limit, compressing with ffmpeg...",
						FrigateEvent.ID, float64(clipSize)/(1024*1024))
					compressedPath, compErr := CompressClip(FilePathClip, FrigateEvent.ID)
					if compErr == nil {
						compInfo, statErr := os.Stat(compressedPath)
						if statErr == nil && compInfo.Size() > 0 && compInfo.Size() < 52428800 {
							log.Info.Printf("Event %s clip compressed successfully from %.2f MB to %.2f MB",
								FrigateEvent.ID, float64(clipSize)/(1024*1024), float64(compInfo.Size())/(1024*1024))
							uploadFilePath = compressedPath
							filesToCleanup = append(filesToCleanup, compressedPath)
							clipSize = compInfo.Size()
						} else {
							log.Warn.Printf("Event %s compressed clip still exceeds limit or is empty", FrigateEvent.ID)
						}
					} else {
						log.Warn.Printf("Event %s ffmpeg compression failed: %v", FrigateEvent.ID, compErr)
					}
				}

				if clipSize < 52428800 {
					log.Debug.Printf("Adding clip to media group: %s (size: %d bytes)", uploadFilePath, clipSize)
					MediaClip := tgbotapi.NewInputMediaVideo(tgbotapi.FilePath(uploadFilePath))

					if !conf.IncludeThumbnailEvent {
						MediaClip.Caption = text
						MediaClip.ParseMode = tgbotapi.ModeMarkdown
					}

					medias = append(medias, MediaClip)
				} else {
					log.Warn.Printf("Event %s clip size (%.2f MB) exceeds Telegram 50MB limit even after compression, skipping direct attachment",
						FrigateEvent.ID, float64(clipSize)/(1024*1024))
					// Add note to message with direct link to full video
					text += fmt.Sprintf("\n⚠️ *Video too large for Telegram (%.1f MB)*: [Watch in Frigate](%s/api/events/%s/clip.mp4)",
						float64(videoInfo.Size())/(1024*1024), conf.FrigateExternalURL, FrigateEvent.ID)
					// Update thumbnail caption if thumbnail is included
					if len(medias) > 0 {
						if photo, ok := medias[0].(tgbotapi.InputMediaPhoto); ok {
							photo.Caption = text
							medias[0] = photo
						}
					}
				}
			}
		}
	}

	// Handle Preview (Video Snippet) - only if no full clip was attached
	var FilePathPreview string

	if conf.IncludePreviewEvent && !hasClip {
		FilePathPreview = SavePreview(FrigateEvent.ID, bot)
		if FilePathPreview != "" {
			filesToCleanup = append(filesToCleanup, FilePathPreview)
			previewInfo, err := os.Stat(FilePathPreview)
			if err == nil && previewInfo.Size() > 0 && previewInfo.Size() < 52428800 {
				log.Debug.Printf("Adding preview to media group: %s (size: %d bytes)", FilePathPreview, previewInfo.Size())
				MediaPreview := tgbotapi.NewInputMediaVideo(tgbotapi.FilePath(FilePathPreview))
				if len(medias) == 0 {
					MediaPreview.Caption = text
					MediaPreview.ParseMode = tgbotapi.ModeMarkdown
				}
				medias = append(medias, MediaPreview)
			} else {
				if err != nil {
					log.Debug.Printf("Error stating preview file: %s", err.Error())
				}
			}
		}
	}

	log.Debug.Printf("Sending media group with %d items", len(medias))

	if len(medias) != 0 {
		// Create message
		msg := tgbotapi.MediaGroupConfig{
			ChatID: conf.TelegramChatID,
			Media:  medias,
		}
		msg.DisableNotification = redis.GetStateMuteEvent()

		var messages []tgbotapi.Message
		var sendErr error
		const maxRetries = 2
		for attempt := 1; attempt <= maxRetries; attempt++ {
			messages, sendErr = bot.SendMediaGroup(msg)
			if sendErr == nil {
				break
			}
			log.Warn.Printf("Event %s: SendMediaGroup attempt %d/%d failed: %v", FrigateEvent.ID, attempt, maxRetries, sendErr)
			if attempt < maxRetries {
				time.Sleep(3 * time.Second)
			}
		}

		if sendErr != nil {
			log.Error.Printf("Failed to send media group for event %s: %s", FrigateEvent.ID, sendErr.Error())
			ErrorSend("Error send media group message: "+sendErr.Error(), bot, FrigateEvent.ID)
			// Do not mark as Finished in Redis on failure!
			// Remove InWork key so next polling cycle can retry
			_ = redis.DelEvent(FrigateEvent.ID)
			return
		}

		if messages == nil {
			ErrorSend("No received messages", bot, FrigateEvent.ID)
		}
	} else {
		msg := tgbotapi.NewMessage(conf.TelegramChatID, "")
		msg.Text = text
		if _, err := bot.Send(msg); err != nil {
			log.Error.Println("Error sending message: " + err.Error())
			_ = redis.DelEvent(FrigateEvent.ID)
			return
		}
	}

	State := "InProgress"
	if FrigateEvent.EndTime != 0 {
		State = "Finished"
	}
	redis.AddNewEvent(FrigateEvent.ID, State, time.Duration(conf.RedisTTL)*time.Second)
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

		if redis.CheckEvent(RedisKeyPrefix + FrigateEvents[Event].ID) {
			if WatchDog {
				SendTextEvent(FrigateEvents[Event], bot)
			} else {
				// Prevent concurrent processing of the same event
				if _, loaded := activeEvents.LoadOrStore(FrigateEvents[Event].ID, struct{}{}); loaded {
					log.Debug.Printf("Event %s is already in active processing, skipping duplicate", FrigateEvents[Event].ID)
					continue
				}

				// Use semaphore to bound concurrent event processing
				eventSemaphore <- struct{}{}
				go func(event EventStruct) {
					defer func() {
						activeEvents.Delete(event.ID)
						<-eventSemaphore
					}()
					SendMessageEvent(event, bot)
				}(FrigateEvents[Event])
			}
		}
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
		select {
		case <-ctx.Done():
			log.Info.Println("NotifyEvents: shutting down watchdog loop.")
			return
		default:
		}
		FrigateEvents := GetEvents(FrigateEventsURL, bot, false)
		ParseEvents(FrigateEvents, bot, true)
		time.Sleep(time.Duration(conf.WatchDogSleepTime) * time.Second)
	}
}

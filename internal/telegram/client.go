package telegram

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// redactedToken replaces the bot token in error messages.
const redactedToken = "<redacted>"

// telegramResponseTimeout is how long to wait for Telegram to answer once a
// request has been sent. It must be longer than the 60s getUpdates long poll.
// Uploads aren't limited: a 50 MB clip can take minutes on a slow uplink.
const telegramResponseTimeout = 2 * time.Minute

// NewBot creates the Telegram bot client.
//
// Telegram request URLs contain the bot token, and Go's HTTP client includes
// the URL in its errors, so every network error would print the token: in the
// library's own retry logs, in our logs, and in error messages sent to the
// chat. The client used here removes the token from errors, and has timeouts
// so a stalled connection can't block an event worker forever.
func NewBot(token string) (*tgbotapi.BotAPI, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = telegramResponseTimeout
	client := &redactingClient{
		client: &http.Client{Transport: transport},
		token:  token,
	}
	bot, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, client)
	if err != nil {
		return nil, redactToken(err, token)
	}
	return bot, nil
}

// redactingClient is an HTTP client that removes the bot token from errors.
type redactingClient struct {
	client *http.Client
	token  string
}

func (c *redactingClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return resp, redactToken(err, c.token)
	}
	return resp, nil
}

// redactToken returns err with every occurrence of token replaced. A
// *url.Error is kept as one, so callers can still check it for timeouts.
func redactToken(err error, token string) error {
	if err == nil || token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	// http.Client.Do returns a *url.Error itself, not wrapped.
	if urlErr, ok := err.(*url.Error); ok {
		redacted := &url.Error{
			Op:  urlErr.Op,
			URL: strings.ReplaceAll(urlErr.URL, token, redactedToken),
			Err: urlErr.Err,
		}
		if !strings.Contains(redacted.Error(), token) {
			return redacted
		}
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, redactedToken))
}

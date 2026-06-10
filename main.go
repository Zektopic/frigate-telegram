package main

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/oldtyt/frigate-telegram/internal/config"
	"github.com/oldtyt/frigate-telegram/internal/frigate"
	"github.com/oldtyt/frigate-telegram/internal/log"
	"github.com/oldtyt/frigate-telegram/internal/redis"
	"github.com/oldtyt/frigate-telegram/internal/restapi"
	"github.com/oldtyt/frigate-telegram/internal/telegram"
)

func main() {
	// Initializing logger
	log.LogFunc()
	// Get config
	conf := config.New()

	// Validate configuration before starting
	if errs := conf.Validate(); len(errs) > 0 {
		for _, e := range errs {
			log.Error.Println("Configuration error: " + e)
		}
		log.Error.Fatalln("Exiting due to configuration errors.")
	}
	if conf.RestAPIEnable && conf.RestAPIKey == "" {
		log.Warn.Println("REST API enabled without REST_API_KEY — API will be unauthenticated!")
	}

	// Prepare startup msg
	startupMsg := "Starting frigate-telegram. "
	startupMsg += "Frigate URL: " + conf.FrigateURL
	log.Info.Println(startupMsg)

	// Set up signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info.Printf("Received signal %v, shutting down gracefully...", sig)
		cancel()
	}()

	if conf.RestAPIEnable {
		go restapi.RunServer(conf)
	}

	// Initializing telegram bot
	bot, err := tgbotapi.NewBotAPI(conf.TelegramBotToken)
	if err != nil {
		log.Error.Fatalln("Error initializing telegram bot: " + err.Error())
	}
	bot.Debug = conf.Debug
	log.Info.Println("Authorized on account " + bot.Self.UserName)

	// Send startup msg.
	_, errmsg := bot.Send(tgbotapi.NewMessage(conf.TelegramChatID, startupMsg))
	if errmsg != nil {
		log.Error.Println(errmsg.Error())
	}

	// Starting ping command handler (healthcheck)
	go telegram.ChatBot(bot, conf)

	FrigateEventsURL := conf.FrigateURL + "/api/events"

	if conf.SendTextEvent {
		go frigate.NotifyEvents(bot, FrigateEventsURL, ctx)
	}

	// Starting loop for getting events from Frigate
	for {
		select {
		case <-ctx.Done():
			log.Info.Println("Shutdown: stopping event polling loop.")
			shutdownMsg := "frigate-telegram shutting down."
			if _, err := bot.Send(tgbotapi.NewMessage(conf.TelegramChatID, shutdownMsg)); err != nil {
				log.Error.Println("Error sending shutdown message: " + err.Error())
			}
			return
		default:
		}

		if redis.GetStateSendEvent() {
			if redis.IsRedisHealthy() {
				events := frigate.GetEvents(FrigateEventsURL, bot, true)
				frigate.ParseEvents(events, bot, false)
			} else {
				log.Debug.Println("Redis circuit breaker open — skipping event processing")
			}
		} else {
			log.Debug.Println("Skipping send events.")
		}
		time.Sleep(time.Duration(conf.SleepTime) * time.Second)
		log.Debug.Println("Sleeping for " + strconv.Itoa(conf.SleepTime) + " seconds.")
	}
}

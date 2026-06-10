package redis

import (
	"context"
	"time"

	"github.com/oldtyt/frigate-telegram/internal/config"
	"github.com/oldtyt/frigate-telegram/internal/log"
	redis "github.com/redis/go-redis/v9"
)

var (
	RedisKeyStateSendEvent string          = "FrigateTelegramStopSendEventMessage"
	RedisKeyStateMuteEvent string          = "FrigateTelegramMuteEventMessage"
	ctx                    context.Context = context.Background()
	conf                   *config.Config  = config.New()
)

var rdb = redis.NewClient(&redis.Options{
	Addr:     conf.RedisAddr,
	Password: conf.RedisPassword, // no password set
	DB:       conf.RedisDB,       // use default DB
	Protocol: conf.RedisProtocol, // specify 2 for RESP 2 or 3 for RESP 3
})

// SetStateSendEvent controls whether event messages are sent.
// Pass true to stop sending, false to resume sending.
// Returns true on success, false if the Redis operation failed.
func SetStateSendEvent(stop bool) bool {
	if stop {
		err := rdb.Set(ctx, RedisKeyStateSendEvent, 1, 0).Err()
		if err != nil {
			log.Error.Println(err)
			return false
		}
		return true
	} else {
		err := rdb.Del(ctx, RedisKeyStateSendEvent).Err()
		if err != nil {
			log.Error.Println("SetStateSendEvent: Redis Del error: " + err.Error())
			return false
		}
		return true
	}
}

// GetStateSendEvent returns whether events are currently being sent.
// Returns true when events are being sent (key is not set).
// Returns false when events are stopped (key is set).
func GetStateSendEvent() bool {
	_, err := rdb.Get(ctx, RedisKeyStateSendEvent).Result()
	//nolint:gosimple
	if err != nil {
		// Key does not exist — events are being sent
		return true
	}
	// Key exists — events are stopped
	return false
}

// IsSendEnabled is a clearer alias for GetStateSendEvent.
// Returns true when event sending is enabled.
func IsSendEnabled() bool {
	return GetStateSendEvent()
}

// Set state notify event msg in redis
func SetStateMuteEvent(mute bool) bool {
	// mute = true - send mute event
	// mute = false - don't mute event msg
	if mute {
		err := rdb.Set(ctx, RedisKeyStateMuteEvent, 1, 0).Err()
		if err != nil {
			log.Error.Println(err)
			return false
		}
		return true
	} else {
		err := rdb.Del(ctx, RedisKeyStateMuteEvent).Err()
		if err != nil {
			log.Error.Println("SetStateMuteEvent: Redis Del error: " + err.Error())
			return false
		}
		return true
	}
}

// Get state send event msg from redis
func GetStateMuteEvent() bool {
	// mute = true - send mute event
	// mute = false - don't mute event msg
	_, err := rdb.Get(ctx, RedisKeyStateMuteEvent).Result()
	//nolint:gosimple
	if err != nil {
		return false
	}
	return true
}

func AddNewEvent(EventID string, State string, RedisTTL time.Duration) {
	err := rdb.Set(ctx, EventID, State, RedisTTL).Err()
	if err != nil {
		log.Error.Println(err)
	}
}

func CheckEvent(EventID string) bool {
	event, err := rdb.Exists(ctx, EventID).Result()
	if err != nil {
		log.Error.Println("CheckEvent: Redis Exists error: " + err.Error())
		// On Redis error, skip the event to avoid spamming duplicates
		return false
	}
	if event == 0 {
		return true
	}
	val, err := rdb.Get(ctx, EventID).Result()
	if err != nil {
		log.Error.Println("CheckEvent: Redis Get error: " + err.Error())
		// On Redis error, skip the event rather than making decisions on empty data
		return false
	}
	if val == "InProgress" {
		return true
	}
	if val == "Finished" {
		return false
	}
	if val == "InWork" {
		return false
	}
	return false
}

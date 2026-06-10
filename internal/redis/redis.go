package redis

import (
	"context"
	"sync"
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

// Circuit breaker for Redis — after consecutive failures, stop trusting Redis
// to prevent event spam when Redis is unreachable.
var (
	consecutiveRedisFailures int
	redisCircuitOpen         bool
	redisCircuitMu           sync.Mutex
)

const maxRedisFailures = 5

func recordRedisFailure() {
	redisCircuitMu.Lock()
	defer redisCircuitMu.Unlock()
	consecutiveRedisFailures++
	if consecutiveRedisFailures >= maxRedisFailures {
		redisCircuitOpen = true
		log.Error.Printf("Redis circuit breaker OPEN after %d consecutive failures", consecutiveRedisFailures)
	}
}

func recordRedisSuccess() {
	redisCircuitMu.Lock()
	defer redisCircuitMu.Unlock()
	if consecutiveRedisFailures > 0 {
		log.Info.Printf("Redis circuit breaker RESET (was %d failures)", consecutiveRedisFailures)
	}
	consecutiveRedisFailures = 0
	redisCircuitOpen = false
}

// IsRedisHealthy returns false if the circuit breaker is open (Redis is down).
func IsRedisHealthy() bool {
	redisCircuitMu.Lock()
	defer redisCircuitMu.Unlock()
	return !redisCircuitOpen
}

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
			recordRedisFailure()
			return false
		}
		recordRedisSuccess()
		return true
	} else {
		err := rdb.Del(ctx, RedisKeyStateSendEvent).Err()
		if err != nil {
			log.Error.Println("SetStateSendEvent: Redis Del error: " + err.Error())
			recordRedisFailure()
			return false
		}
		recordRedisSuccess()
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
	recordRedisSuccess()
	return false
}

// IsSendEnabled is a clearer alias for GetStateSendEvent.
// Returns true when event sending is enabled.
func IsSendEnabled() bool {
	return GetStateSendEvent()
}

// Set state notify event msg in redis
func SetStateMuteEvent(mute bool) bool {
	if mute {
		err := rdb.Set(ctx, RedisKeyStateMuteEvent, 1, 0).Err()
		if err != nil {
			log.Error.Println(err)
			recordRedisFailure()
			return false
		}
		recordRedisSuccess()
		return true
	} else {
		err := rdb.Del(ctx, RedisKeyStateMuteEvent).Err()
		if err != nil {
			log.Error.Println("SetStateMuteEvent: Redis Del error: " + err.Error())
			recordRedisFailure()
			return false
		}
		recordRedisSuccess()
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
	recordRedisSuccess()
	return true
}

func AddNewEvent(EventID string, State string, RedisTTL time.Duration) {
	err := rdb.Set(ctx, EventID, State, RedisTTL).Err()
	if err != nil {
		log.Error.Println(err)
		recordRedisFailure()
		return
	}
	recordRedisSuccess()
}

func CheckEvent(EventID string) bool {
	event, err := rdb.Exists(ctx, EventID).Result()
	if err != nil {
		log.Error.Println("CheckEvent: Redis Exists error: " + err.Error())
		recordRedisFailure()
		return false
	}
	if event == 0 {
		recordRedisSuccess()
		return true
	}
	val, err := rdb.Get(ctx, EventID).Result()
	if err != nil {
		log.Error.Println("CheckEvent: Redis Get error: " + err.Error())
		recordRedisFailure()
		return false
	}
	recordRedisSuccess()
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

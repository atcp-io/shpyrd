// The shop worker: takes orders from the Redis queue the web process fills
// and "fulfils" them, logging structured lines as it goes.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

func main() {
	u := os.Getenv("REDIS_URL")
	if u == "" {
		log.Println(`{"level":"warn","msg":"no REDIS_URL: attach a Redis (shpyrd attach cache); idling"}`)
		for {
			time.Sleep(time.Minute)
		}
	}
	opt, err := redis.ParseURL(u)
	if err != nil {
		log.Fatalf("redis: %v", err)
	}
	rdb := redis.NewClient(opt)
	ctx := context.Background()
	host, _ := os.Hostname()
	log.Printf(`{"level":"info","msg":"worker started","instance":%q}`, host)
	for {
		res, err := rdb.BLPop(ctx, 30*time.Second, "orders").Result()
		if err == redis.Nil {
			log.Println(`{"level":"debug","msg":"queue empty"}`)
			continue
		}
		if err != nil {
			log.Printf(`{"level":"error","msg":"queue read failed","error":%q}`, err.Error())
			time.Sleep(2 * time.Second)
			continue
		}
		start := time.Now()
		time.Sleep(150 * time.Millisecond) // pretend to pack the order
		rdb.Incr(ctx, "orders:fulfilled")
		log.Printf(`{"level":"info","msg":"order fulfilled","order":%q,"duration_ms":%d}`, res[1], time.Since(start).Milliseconds())
	}
}

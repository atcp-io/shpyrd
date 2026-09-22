// The worker process of the hello-world example: a background job that
// wakes up every INTERVAL (default 10s), does some "work" and logs it. It
// reads the same config vars as the web process (GREETING).
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	interval := 10 * time.Second
	if v := os.Getenv("INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			interval = d
		}
	}
	greeting := os.Getenv("GREETING")
	if greeting == "" {
		greeting = "Hello world"
	}
	host, _ := os.Hostname()
	log.Printf("worker started on %s (interval %s)", host, interval)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for n := 1; ; n++ {
		select {
		case <-ticker.C:
			start := time.Now()
			time.Sleep(200 * time.Millisecond) // pretend to work
			log.Printf("job #%d done in %s: %q", n, time.Since(start).Round(time.Millisecond), greeting)
		case sig := <-stop:
			log.Printf("worker stopping (%s) after %d jobs", sig, n-1)
			return
		}
	}
}

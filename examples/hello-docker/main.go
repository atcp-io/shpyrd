// The hello-docker example: the same "Hello world" service as examples/hello,
// built from a Dockerfile instead of buildpacks. `main web` serves HTTP,
// `main worker` logs a heartbeat, so the worker process only needs a command.
package main

import (
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	role := "web"
	if len(os.Args) > 1 {
		role = os.Args[1]
	}
	host, _ := os.Hostname()
	switch role {
	case "worker":
		for i := 1; ; i++ {
			log.Printf("worker heartbeat #%d from %s", i, host)
			time.Sleep(10 * time.Second)
		}
	default:
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			greeting := os.Getenv("GREETING")
			if greeting == "" {
				greeting = "Hello from a Dockerfile"
			}
			log.Printf("%s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, "<!doctype html><title>%[1]s</title><h1>%[1]s</h1><p>served by <code>%[2]s</code>, built with <code>%[3]s</code></p>",
				html.EscapeString(greeting), html.EscapeString(host), html.EscapeString(os.Getenv("BUILT_WITH")))
		})
		log.Printf("web listening on :%s", port)
		log.Fatal(http.ListenAndServe(":"+port, nil))
	}
}

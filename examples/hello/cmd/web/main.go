// The web process of the hello-world example: an HTML "Hello world" page that
// shows who is visiting (verified from the platform's JWT), which instance
// served the request and a GREETING config var. See ../../README.md.
package main

import (
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

var requests atomic.Int64

func main() {
	// Buildpack-built apps get PORT from the platform (shpyrd sets 8080).
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	host, _ := os.Hostname()
	started := time.Now()
	jwt := newVerifier()
	if jwt.configured() {
		log.Printf("verifying visitors against %s/.well-known/jwks.json (audience %s)", jwt.issuer, jwt.audience)
	}

	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		n := requests.Add(1)
		greeting := os.Getenv("GREETING")
		if greeting == "" {
			greeting = "Hello world"
		}
		log.Printf("%s %s from %s (request #%d)", r.Method, r.URL.Path, r.RemoteAddr, n)
		// Behind shpyrd's edge the visitor arrives identified: no sign-in
		// code in the app. The JWT proves it; the headers merely repeat it.
		who, how := "an anonymous visitor", "no identity: the app is public or the request did not come through the edge"
		if jwt.configured() {
			if v, err := jwt.verify(r); err == nil {
				who = v.Email
				if who == "" {
					who = v.Subject
				}
				if len(v.Teams) > 0 {
					who += " (teams: " + strings.Join(v.Teams, ",") + ")"
				}
				how = fmt.Sprintf("JWT verified against %s (roles: %s, expires in %s)", jwt.issuer, strings.Join(v.Roles, ","), time.Until(time.Unix(v.ExpiresAt, 0)).Round(time.Second))
			} else if r.Header.Get("Authorization") != "" {
				how = "JWT rejected: " + err.Error()
			}
		} else if u := r.Header.Get("X-Shpyrd-User"); u != "" {
			who = u
			if teams := r.Header.Get("X-Shpyrd-Teams"); teams != "" {
				who += " (teams: " + teams + ")"
			}
			how = "from the X-Shpyrd-* headers (SHPYRD_ISSUER is not set, so the JWT was not checked)"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, page,
			html.EscapeString(greeting),
			html.EscapeString(greeting),
			html.EscapeString(who),
			html.EscapeString(how),
			html.EscapeString(host),
			n,
			time.Since(started).Round(time.Second),
			time.Now().UTC().Format(time.RFC3339),
		)
	})

	log.Printf("hello-world listening on :%s (instance %s)", port, host)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

const page = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>%s</title>
  <style>
    :root { color-scheme: light dark; }
    body { margin: 0; min-height: 100vh; display: grid; place-items: center;
           font-family: system-ui, sans-serif; background: #0f172a; color: #e2e8f0; }
    main { text-align: center; padding: 2rem; }
    h1 { font-size: clamp(2.5rem, 8vw, 5rem); margin: 0 0 1rem; letter-spacing: -0.03em; }
    p { color: #94a3b8; margin: 0.25rem 0; }
    code { background: #1e293b; padding: 0.15rem 0.4rem; border-radius: 0.3rem; color: #f8fafc; }
    .how { font-size: 0.85rem; color: #64748b; max-width: 40rem; }
    .tag { display: inline-block; margin-top: 1.5rem; padding: 0.3rem 0.7rem; border-radius: 999px;
           background: #14532d; color: #bbf7d0; font-size: 0.85rem; }
  </style>
</head>
<body>
  <main>
    <h1>%s</h1>
    <p>You are <code>%s</code></p>
    <p class="how">%s</p>
    <p>Served by instance <code>%s</code></p>
    <p>Request #%d on this instance &middot; up for %s</p>
    <p>%s</p>
    <span class="tag">built with Paketo buildpacks, deployed by shpyrd</span>
  </main>
</body>
</html>
`

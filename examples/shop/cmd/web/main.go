// The shop example: a small storefront that uses an attached Postgres
// (DATABASE_URL) for products and orders and an attached Redis (REDIS_URL)
// as an order queue and counter. Without the attachments it still runs and
// says what is missing, so the attach flow is visible in the UI.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

type product struct {
	ID    int
	Name  string
	Price float64
}

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>shop</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:720px;margin:3rem auto;padding:0 1rem;color:#0f172a}
h1{color:#ff4f00}table{border-collapse:collapse;width:100%}td,th{padding:.5rem;border-bottom:1px solid #e2e8f0;text-align:left}
.muted{color:#64748b}button{background:#ff4f00;color:#fff;border:0;padding:.5rem 1rem;border-radius:6px;font-size:1rem}
.warn{background:#fff7ed;border:1px solid #fed7aa;padding:.75rem 1rem;border-radius:8px}</style></head>
<body><h1>shop</h1>
<p class="muted">release {{.Release}} · served by <code>{{.Host}}</code></p>
{{if .Warning}}<p class="warn">{{.Warning}}</p>{{end}}
{{if .Products}}<table><tr><th>Product</th><th>Price</th></tr>
{{range .Products}}<tr><td>{{.Name}}</td><td>${{printf "%.2f" .Price}}</td></tr>{{end}}</table>{{end}}
<p>Orders placed: <strong>{{.Orders}}</strong> · queue depth: <strong>{{.Queue}}</strong></p>
<form method="post" action="/order"><button>Place an order</button></form>
</body></html>`))

type app struct {
	db  *sql.DB
	rdb *redis.Client
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	a := &app{}
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			log.Fatalf("database: %v", err)
		}
		a.db = db
		if err := a.migrate(); err != nil {
			log.Printf("database not ready yet: %v", err)
		}
	}
	if u := os.Getenv("REDIS_URL"); u != "" {
		opt, err := redis.ParseURL(u)
		if err != nil {
			log.Fatalf("redis: %v", err)
		}
		a.rdb = redis.NewClient(opt)
	}
	host, _ := os.Hostname()
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	http.HandleFunc("/api/products", func(w http.ResponseWriter, r *http.Request) {
		products, err := a.products(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(products)
	})
	http.HandleFunc("/order", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if a.rdb == nil {
			http.Error(w, "no Redis attached: shpyrd attach cache", http.StatusServiceUnavailable)
			return
		}
		id := time.Now().UnixNano()
		pipe := a.rdb.Pipeline()
		pipe.Incr(r.Context(), "orders:count")
		pipe.RPush(r.Context(), "orders", strconv.FormatInt(id, 10))
		if _, err := pipe.Exec(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		log.Printf("order %d queued", id)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		start := time.Now()
		data := map[string]any{"Host": host, "Release": os.Getenv("SHPYRD_RELEASE"), "Orders": "-", "Queue": "-"}
		if products, err := a.products(r.Context()); err != nil {
			data["Warning"] = "No database attached yet: run `shpyrd pg create db` and `shpyrd attach db`."
		} else {
			data["Products"] = products
		}
		if a.rdb != nil {
			data["Orders"], _ = a.rdb.Get(r.Context(), "orders:count").Result()
			data["Queue"], _ = a.rdb.LLen(r.Context(), "orders").Result()
		} else if data["Warning"] == nil {
			data["Warning"] = "No Redis attached yet: run `shpyrd redis create cache` and `shpyrd attach cache`."
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.Execute(w, data)
		log.Printf(`{"level":"info","msg":"request","method":%q,"path":%q,"status":200,"duration_ms":%d,"remote":%q}`, r.Method, r.URL.Path, time.Since(start).Milliseconds(), r.RemoteAddr)
	})
	log.Printf("web listening on :%s (database: %v, redis: %v)", port, a.db != nil, a.rdb != nil)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func (a *app) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.db.ExecContext(ctx, `create table if not exists products (id serial primary key, name text not null, price numeric(10,2) not null)`); err != nil {
		return err
	}
	var n int
	if err := a.db.QueryRowContext(ctx, `select count(*) from products`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		_, err := a.db.ExecContext(ctx, `insert into products (name, price) values ('Espresso', 3.50), ('Cappuccino', 4.20), ('Croissant', 2.80), ('Cold brew', 4.90)`)
		return err
	}
	return nil
}

func (a *app) products(ctx context.Context) ([]product, error) {
	if a.db == nil {
		return nil, fmt.Errorf("no database")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := a.db.QueryContext(ctx, `select id, name, price from products order by id`)
	if err != nil {
		if merr := a.migrate(); merr == nil {
			return a.products(ctx)
		}
		return nil, err
	}
	defer rows.Close()
	var out []product
	for rows.Next() {
		var p product
		if err := rows.Scan(&p.ID, &p.Name, &p.Price); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// The blog example (Node.js, buildpacks): a few posts, a visit counter kept
// on a persistent volume (/data) so it survives deploys, and structured logs.
const http = require('node:http')
const fs = require('node:fs')
const path = require('node:path')

const port = process.env.PORT || 8080
const dataDir = process.env.DATA_DIR || '/data'
const counterFile = path.join(dataDir, 'visits.txt')
const host = require('node:os').hostname()

const posts = [
  { slug: 'hello', title: 'Hello from shpyrd', body: 'Deployed from source with buildpacks: no Dockerfile, no YAML.' },
  { slug: 'volumes', title: 'Counting visits on a volume', body: 'This page keeps its visit counter in /data, a single-instance volume that outlives deploys.' },
  { slug: 'logs', title: 'Structured logs', body: 'Every request is logged as JSON; the dashboard renders level and message.' },
]

function visits(increment) {
  try {
    fs.mkdirSync(dataDir, { recursive: true })
    const n = Number(fs.existsSync(counterFile) ? fs.readFileSync(counterFile, 'utf8') : 0) + (increment ? 1 : 0)
    if (increment) fs.writeFileSync(counterFile, String(n))
    return { n, persistent: true }
  } catch (err) {
    return { n: 0, persistent: false, error: err.message }
  }
}

const page = (body) => `<!doctype html><html><head><meta charset="utf-8"><title>blog</title>
<style>body{font:16px/1.6 system-ui,sans-serif;max-width:680px;margin:3rem auto;padding:0 1rem;color:#0f172a}h1,h2{color:#ff4f00}a{color:#0f172a}.muted{color:#64748b}</style>
</head><body>${body}</body></html>`

http.createServer((req, res) => {
  const start = Date.now()
  const done = (status, type, body) => {
    res.writeHead(status, { 'content-type': type })
    res.end(body)
    console.log(JSON.stringify({ level: status >= 500 ? 'error' : 'info', msg: 'request', method: req.method, path: req.url, status, duration_ms: Date.now() - start }))
  }
  if (req.url === '/healthz') return done(200, 'text/plain', 'ok')
  if (req.url === '/') {
    const v = visits(true)
    const list = posts.map((p) => `<li><a href="/posts/${p.slug}">${p.title}</a></li>`).join('')
    const note = v.persistent ? `visit #${v.n} (counter on the volume)` : `no volume mounted (${v.error})`
    return done(200, 'text/html; charset=utf-8', page(`<h1>blog</h1><p class="muted">${note} · served by <code>${host}</code></p><ul>${list}</ul>`))
  }
  const m = req.url.match(/^\/posts\/([a-z-]+)$/)
  const post = m && posts.find((p) => p.slug === m[1])
  if (post) return done(200, 'text/html; charset=utf-8', page(`<p><a href="/">← blog</a></p><h2>${post.title}</h2><p>${post.body}</p>`))
  done(404, 'text/plain', 'not found')
}).listen(port, () => console.log(JSON.stringify({ level: 'info', msg: 'blog listening', port: Number(port), dataDir })))

package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"shpyrd/pkg/ext"
)

// State behind the web terminal (RFC-0026): one-time tickets that
// authenticate a WebSocket, and the registry that holds a user to one shell
// per project.

// execTicketTTL is how long a minted ticket may be redeemed. It is short
// because the ticket is a bearer credential: the WebSocket presents nothing
// else.
const execTicketTTL = 30 * time.Second

// execTicket is what redeeming a code proves. It carries the actor because a
// browser cannot set headers on a WebSocket, and `shpyrd cluster dashboard`
// signs in with a token in localStorage and so has no session cookie either.
type execTicket struct {
	Identity ext.Identity
	Project  string
	Instance string
	expires  time.Time
}

// ticketStore holds unredeemed tickets. In memory is enough: they live for
// 30 seconds and the server runs a single replica
// (deploy/components/shpyrd/base/server.yaml).
type ticketStore struct {
	mu  sync.Mutex
	m   map[string]execTicket // keyed by the code's hash
	ttl time.Duration
	now func() time.Time
}

func newTicketStore(ttl time.Duration) *ticketStore {
	return &ticketStore{m: map[string]execTicket{}, ttl: ttl, now: time.Now}
}

// mint stores t and returns the code to put in the WebSocket's query. Only
// the hash is kept, so a heap dump or an accidental log of the store does not
// hand out working tickets.
func (st *ticketStore) mint(t execTicket) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked()
	t.expires = st.now().Add(st.ttl)
	st.m[hashCode(code)] = t
	return code, nil
}

// redeem consumes the ticket for code. One shot: gone whether it was valid or
// merely stale.
func (st *ticketStore) redeem(code string) (execTicket, error) {
	if code == "" {
		return execTicket{}, errors.New("no shell ticket presented")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked()
	key := hashCode(code)
	t, ok := st.m[key]
	delete(st.m, key)
	if !ok || st.now().After(t.expires) {
		return execTicket{}, errors.New("the shell ticket is invalid or expired; open the Shell tab again")
	}
	return t, nil
}

// sweepLocked drops expired tickets so an unredeemed one cannot accumulate.
func (st *ticketStore) sweepLocked() {
	now := st.now()
	for k, t := range st.m {
		if now.After(t.expires) {
			delete(st.m, k)
		}
	}
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// shellRegistry enforces one shell per user per project (RFC-0026). In memory
// is correct only while the server runs a single replica
// (deploy/components/shpyrd/base/server.yaml); scaling out moves this into
// the control-plane store.
type shellRegistry struct {
	mu   sync.Mutex
	live map[string]struct{}
}

func newShellRegistry() *shellRegistry {
	return &shellRegistry{live: map[string]struct{}{}}
}

func shellKey(actor, project string) string { return actor + "\x00" + project }

// claim takes the slot, reporting false when a shell is already live.
func (r *shellRegistry) claim(actor, project string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := shellKey(actor, project)
	if _, ok := r.live[k]; ok {
		return false
	}
	r.live[k] = struct{}{}
	return true
}

func (r *shellRegistry) release(actor, project string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, shellKey(actor, project))
}

func (r *shellRegistry) held(actor, project string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.live[shellKey(actor, project)]
	return ok
}

// actorKey identifies the person the limit applies to. Subject alone is not
// enough: two providers can issue the same one.
func actorKey(id ext.Identity) string { return id.Provider + "\x00" + id.Subject }

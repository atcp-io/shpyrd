package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"shpyrd/pkg/ext"
	"shpyrd/pkg/store"
)

// Sessions (RFC-0007) live in the control-plane store since RFC-0033
// phase 3, so a restart keeps people signed in and every replica sees the
// same sign-outs. The server keeps a short-lived cache in front of it: a
// hit costs nothing, a miss one query, and an entry is re-checked against
// the store every cacheRevalidate so a sign-out on another replica takes
// effect within that window. Installs made earlier kept sessions in Secret
// shpyrd-sessions; the first start imports them once.

// SessionsSecretName is the pre-store mirror (imported, then left alone).
const SessionsSecretName = "shpyrd-sessions"

const (
	sessionIdle     = 12 * time.Hour
	sessionAbsolute = 7 * 24 * time.Hour
	cacheRevalidate = 30 * time.Second
	touchEvery      = time.Minute
)

type session struct {
	ID string `json:"id"`
	// WorkspaceID is the workspace the session was opened in: a cookie is
	// bound to its host, and the host to a workspace, so a session never
	// answers for another workspace (RFC-0033 phase 6).
	WorkspaceID string       `json:"workspaceId,omitempty"`
	CSRF        string       `json:"csrf"`
	Identity    ext.Identity `json:"identity"`
	CreatedAt   time.Time    `json:"createdAt"`
	LastSeen    time.Time    `json:"lastSeen"`
	// IDToken is kept only when the issuer supports RP-initiated logout,
	// to pass as id_token_hint when the session ends (RFC-0012).
	IDToken string `json:"idToken,omitempty"`
}

func (s *session) expired(now time.Time) bool {
	return now.Sub(s.CreatedAt) > sessionAbsolute || now.Sub(s.LastSeen) > sessionIdle
}

type cachedSession struct {
	s         *session
	checkedAt time.Time
	touchedAt time.Time
}

type sessionStore struct {
	mu    sync.Mutex
	cache map[string]*cachedSession
	store store.Store
	ws    string               // the implicit workspace: where the legacy mirror is imported
	kube  kubernetes.Interface // nil: no Secret to import from (tests)
	ns    string
	log   *slog.Logger
	now   func() time.Time
}

func newSessionStore(st store.Store, kube kubernetes.Interface, ns string, log *slog.Logger) *sessionStore {
	return &sessionStore{cache: map[string]*cachedSession{}, store: st, ws: store.DefaultWorkspace, kube: kube, ns: ns, log: log, now: time.Now}
}

// load imports the pre-store Secret mirror once: only when the store holds
// no sessions yet, so an upgrade keeps everyone signed in.
func (st *sessionStore) load(ctx context.Context) {
	if st.kube == nil {
		return
	}
	if n, err := st.store.CountSessions(ctx, st.ws); err != nil || n > 0 {
		return
	}
	sec, err := st.kube.CoreV1().Secrets(st.ns).Get(ctx, SessionsSecretName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			st.log.Warn("sessions: cannot read the old mirror", "error", err)
		}
		return
	}
	var list []*session
	if err := json.Unmarshal(sec.Data["sessions"], &list); err != nil {
		return
	}
	now := st.now()
	imported := 0
	for _, s := range list {
		if s.expired(now) {
			continue
		}
		if err := st.store.PutSession(ctx, st.ws, toStoreSession(s)); err == nil {
			imported++
		}
	}
	if imported > 0 {
		st.log.Info("sessions imported from the Secret mirror into the control-plane store", "count", imported)
	}
	// The mirror is history from here on.
	_ = st.kube.CoreV1().Secrets(st.ns).Delete(ctx, SessionsSecretName, metav1.DeleteOptions{})
}

func toStoreSession(s *session) store.Session {
	id, _ := json.Marshal(s.Identity)
	return store.Session{ID: s.ID, Identity: id, CSRF: s.CSRF, IDToken: s.IDToken, CreatedAt: s.CreatedAt, LastSeenAt: s.LastSeen}
}

func fromStoreSession(s *store.Session) *session {
	out := &session{ID: s.ID, WorkspaceID: s.WorkspaceID, CSRF: s.CSRF, IDToken: s.IDToken, CreatedAt: s.CreatedAt, LastSeen: s.LastSeenAt}
	_ = json.Unmarshal(s.Identity, &out.Identity)
	return out
}

// create opens a session in a workspace (slug; "" means the implicit one).
func (st *sessionStore) create(ctx context.Context, ws string, id ext.Identity, idToken string) (*session, error) {
	sid, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	if ws == "" {
		ws = st.ws
	}
	now := st.now()
	s := &session{ID: sid, CSRF: csrf, Identity: id, CreatedAt: now, LastSeen: now, IDToken: idToken}
	if err := st.store.PutSession(ctx, ws, toStoreSession(s)); err != nil {
		return nil, fmt.Errorf("store session: %w", err)
	}
	if stored, err := st.store.GetSession(ctx, sid); err == nil {
		s.WorkspaceID = stored.WorkspaceID
	}
	st.mu.Lock()
	st.cache[sid] = &cachedSession{s: s, checkedAt: now, touchedAt: now}
	st.mu.Unlock()
	// Housekeeping, off the request's path.
	go func() {
		pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = st.store.PurgeSessions(pctx, now.Add(-sessionAbsolute), now.Add(-sessionIdle))
	}()
	return s, nil
}

// getIn is get restricted to sessions of one workspace (by id): a cookie
// presented at the wrong host is no session at all.
func (st *sessionStore) getIn(id, workspaceID string) (*session, bool) {
	s, ok := st.get(id)
	if !ok || (workspaceID != "" && s.WorkspaceID != "" && s.WorkspaceID != workspaceID) {
		return nil, false
	}
	return s, true
}

// get returns a live session and marks it seen. The cache answers within
// cacheRevalidate; then the store is asked again.
func (st *sessionStore) get(id string) (*session, bool) {
	now := st.now()
	st.mu.Lock()
	c, ok := st.cache[id]
	if ok && now.Sub(c.checkedAt) < cacheRevalidate {
		if c.s.expired(now) {
			delete(st.cache, id)
			st.mu.Unlock()
			return nil, false
		}
		c.s.LastSeen = now
		touch := now.Sub(c.touchedAt) >= touchEvery
		if touch {
			c.touchedAt = now
		}
		s := c.s
		st.mu.Unlock()
		if touch {
			st.touch(id, now)
		}
		return s, true
	}
	st.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stored, err := st.store.GetSession(ctx, id)
	if err != nil {
		st.mu.Lock()
		delete(st.cache, id)
		st.mu.Unlock()
		return nil, false
	}
	s := fromStoreSession(stored)
	if s.expired(now) {
		_ = st.store.DeleteSession(ctx, id)
		st.mu.Lock()
		delete(st.cache, id)
		st.mu.Unlock()
		return nil, false
	}
	s.LastSeen = now
	st.mu.Lock()
	st.cache[id] = &cachedSession{s: s, checkedAt: now, touchedAt: now}
	st.mu.Unlock()
	st.touch(id, now)
	return s, true
}

func (st *sessionStore) touch(id string, at time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.store.TouchSession(ctx, id, at); err != nil && st.log != nil {
		st.log.Debug("session touch failed", "error", err)
	}
}

func (st *sessionStore) delete(ctx context.Context, id string) {
	st.mu.Lock()
	delete(st.cache, id)
	st.mu.Unlock()
	if err := st.store.DeleteSession(ctx, id); err != nil && st.log != nil {
		st.log.Warn("sessions: cannot delete", "error", err)
	}
}

// count is used by tests and the cluster page.
func (st *sessionStore) count() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, _ := st.store.CountSessions(ctx, st.ws)
	return n
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

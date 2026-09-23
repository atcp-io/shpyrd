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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"shpyrd/pkg/ext"
)

// Sessions of signed-in dashboard users (RFC-0007). They are kept in memory
// and mirrored into a Secret so a server restart does not sign everyone
// out. A session holds the identity, never the issuer's access or refresh
// tokens; the id_token is kept only for issuers that support RP-initiated
// logout (RFC-0012).

// SessionsSecretName is the Secret mirroring the sessions.
const SessionsSecretName = "shpyrd-sessions"

const (
	sessionIdle     = 12 * time.Hour
	sessionAbsolute = 7 * 24 * time.Hour
)

type session struct {
	ID        string       `json:"id"`
	CSRF      string       `json:"csrf"`
	Identity  ext.Identity `json:"identity"`
	CreatedAt time.Time    `json:"createdAt"`
	LastSeen  time.Time    `json:"lastSeen"`
	// IDToken is kept only when the issuer supports RP-initiated logout,
	// to pass as id_token_hint when the session ends (RFC-0012).
	IDToken string `json:"idToken,omitempty"`
}

func (s *session) expired(now time.Time) bool {
	return now.Sub(s.CreatedAt) > sessionAbsolute || now.Sub(s.LastSeen) > sessionIdle
}

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*session
	kube     kubernetes.Interface // nil: memory only (tests)
	ns       string
	log      *slog.Logger
	now      func() time.Time
}

func newSessionStore(kube kubernetes.Interface, ns string, log *slog.Logger) *sessionStore {
	return &sessionStore{sessions: map[string]*session{}, kube: kube, ns: ns, log: log, now: time.Now}
}

// load reads the mirrored sessions; missing Secret means a fresh start.
func (st *sessionStore) load(ctx context.Context) {
	if st.kube == nil {
		return
	}
	sec, err := st.kube.CoreV1().Secrets(st.ns).Get(ctx, SessionsSecretName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			st.log.Warn("sessions: cannot read mirror", "error", err)
		}
		return
	}
	var list []*session
	if err := json.Unmarshal(sec.Data["sessions"], &list); err != nil {
		st.log.Warn("sessions: corrupt mirror, starting empty", "error", err)
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	for _, s := range list {
		if !s.expired(now) {
			st.sessions[s.ID] = s
		}
	}
}

// persist writes the current sessions to the Secret (best effort).
func (st *sessionStore) persist(ctx context.Context) {
	if st.kube == nil {
		return
	}
	st.mu.Lock()
	list := make([]*session, 0, len(st.sessions))
	for _, s := range st.sessions {
		list = append(list, s)
	}
	st.mu.Unlock()
	raw, _ := json.Marshal(list)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SessionsSecretName, Namespace: st.ns, Labels: map[string]string{"app.kubernetes.io/managed-by": "shpyrd"}},
		Data:       map[string][]byte{"sessions": raw},
	}
	_, err := st.kube.CoreV1().Secrets(st.ns).Update(ctx, sec, metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) {
		_, err = st.kube.CoreV1().Secrets(st.ns).Create(ctx, sec, metav1.CreateOptions{})
	}
	if err != nil {
		st.log.Warn("sessions: cannot persist", "error", err)
	}
}

func (st *sessionStore) create(ctx context.Context, id ext.Identity, idToken string) (*session, error) {
	sid, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	now := st.now()
	s := &session{ID: sid, CSRF: csrf, Identity: id, CreatedAt: now, LastSeen: now, IDToken: idToken}
	st.mu.Lock()
	st.sessions[sid] = s
	// Housekeeping while we hold the lock.
	for k, v := range st.sessions {
		if v.expired(now) {
			delete(st.sessions, k)
		}
	}
	st.mu.Unlock()
	st.persist(ctx)
	return s, nil
}

// get returns a live session and marks it seen.
func (st *sessionStore) get(id string) (*session, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[id]
	if !ok {
		return nil, false
	}
	now := st.now()
	if s.expired(now) {
		delete(st.sessions, id)
		return nil, false
	}
	s.LastSeen = now
	return s, true
}

func (st *sessionStore) delete(ctx context.Context, id string) {
	st.mu.Lock()
	delete(st.sessions, id)
	st.mu.Unlock()
	st.persist(ctx)
}

// count is used by tests and the cluster page.
func (st *sessionStore) count() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.sessions)
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

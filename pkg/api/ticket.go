package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// One-time login tickets let someone with cluster access open the dashboard
// without carrying the admin token into the browser: the CLI stores the hash
// of a random code in a short-lived Secret, the browser presents the code
// once, the server deletes the Secret and starts a normal session.

// LoginTicketLabel marks ticket Secrets.
const LoginTicketLabel = "shpyrd.io/login-ticket"

// LoginTicketTTL is how long a ticket may be redeemed.
const LoginTicketTTL = 60 * time.Second

// MintLoginTicket creates a ticket for actor and returns the code to put in
// the URL.
func MintLoginTicket(ctx context.Context, k kubernetes.Interface, namespace, actor string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(code))
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shpyrd-login-" + hex.EncodeToString(suffix),
			Namespace: namespace,
			Labels:    map[string]string{LoginTicketLabel: "true", "app.kubernetes.io/managed-by": "shpyrd"},
		},
		Data: map[string][]byte{
			"hash":    []byte(hex.EncodeToString(sum[:])),
			"expires": []byte(time.Now().Add(LoginTicketTTL).UTC().Format(time.RFC3339)),
			"actor":   []byte(actor),
		},
	}
	if _, err := k.CoreV1().Secrets(namespace).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("create login ticket: %w", err)
	}
	return code, nil
}

// RedeemLoginTicket consumes the ticket matching code and returns its actor.
// Expired tickets are removed as they are met.
func RedeemLoginTicket(ctx context.Context, k kubernetes.Interface, namespace, code string) (string, error) {
	list, err := k.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{LabelSelector: LoginTicketLabel + "=true"})
	if err != nil {
		return "", fmt.Errorf("list login tickets: %w", err)
	}
	sum := sha256.Sum256([]byte(code))
	want := []byte(hex.EncodeToString(sum[:]))
	now := time.Now()
	var actor string
	found := false
	for _, sec := range list.Items {
		expires, perr := time.Parse(time.RFC3339, string(sec.Data["expires"]))
		expired := perr != nil || now.After(expires)
		match := subtle.ConstantTimeCompare(sec.Data["hash"], want) == 1
		if expired || match {
			// One shot: gone whether it was used or merely stale.
			_ = k.CoreV1().Secrets(namespace).Delete(ctx, sec.Name, metav1.DeleteOptions{})
		}
		if match && !expired {
			found, actor = true, string(sec.Data["actor"])
		}
	}
	if !found {
		return "", errors.New("login ticket is invalid or expired; run `shpyrd cluster dashboard` again")
	}
	return actor, nil
}

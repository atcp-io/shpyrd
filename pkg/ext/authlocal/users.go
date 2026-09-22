package authlocal

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"net/mail"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Local accounts are Dex "Password" objects (its Kubernetes storage): email,
// bcrypt hash, display name and a stable user id. Dex renders the login
// form and checks the password; shpyrd only manages the objects, so no Dex
// API client is needed and users survive Dex restarts and upgrades.

// PasswordGVR is Dex's password resource.
var PasswordGVR = schema.GroupVersionResource{Group: "dex.coreos.com", Version: "v1", Resource: "passwords"}

// MinPasswordLength is enforced on create and change.
const MinPasswordLength = 8

// ErrNotEnabled says Dex (and its CRDs) are not installed.
var ErrNotEnabled = errors.New("local accounts are not enabled: run `shpyrd extensions enable auth-local`")

// User is a local account as shown to admins.
type User struct {
	Email     string    `json:"email"`
	Name      string    `json:"name,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// nameEncoding is the alphabet Dex uses to turn ids into object names.
var nameEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567")

// PasswordName is the object name Dex expects for an email: its kubernetes
// storage appends the FNV-64 offset basis to the lower-cased id and base32
// encodes it (see dex/storage/kubernetes/client.go idToName).
func PasswordName(email string) string {
	sum := fnv.New64().Sum([]byte(strings.ToLower(email)))
	return strings.TrimRight(nameEncoding.EncodeToString(sum), "=")
}

// NormalizeEmail validates and lower-cases an address.
func NormalizeEmail(email string) (string, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", fmt.Errorf("invalid email address %q", email)
	}
	return email, nil
}

// CheckPassword applies the password policy.
func CheckPassword(pw string) error {
	if len(pw) < MinPasswordLength {
		return fmt.Errorf("password must have at least %d characters", MinPasswordLength)
	}
	return nil
}

// Store manages local accounts in a namespace.
type Store struct {
	Dynamic   dynamic.Interface
	Namespace string
}

func (s *Store) res() dynamic.ResourceInterface {
	return s.Dynamic.Resource(PasswordGVR).Namespace(s.Namespace)
}

// List returns the accounts sorted by email.
func (s *Store) List(ctx context.Context) ([]User, error) {
	list, err := s.res().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]User, 0, len(list.Items))
	for _, u := range list.Items {
		email, _, _ := unstructured.NestedString(u.Object, "email")
		name, _, _ := unstructured.NestedString(u.Object, "username")
		out = append(out, User{Email: email, Name: name, CreatedAt: u.GetCreationTimestamp().Time})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

// Create adds an account; the email must be new.
func (s *Store) Create(ctx context.Context, email, name, password string) (*User, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	if err := CheckPassword(password); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	idRaw := make([]byte, 16)
	if _, err := rand.Read(idRaw); err != nil {
		return nil, err
	}
	if name == "" {
		name = strings.SplitN(email, "@", 2)[0]
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": PasswordGVR.Group + "/" + PasswordGVR.Version,
		"kind":       "Password",
		"metadata": map[string]interface{}{
			"name":      PasswordName(email),
			"namespace": s.Namespace,
			"labels":    map[string]interface{}{"app.kubernetes.io/managed-by": "shpyrd"},
		},
		"email":    email,
		"hash":     base64.StdEncoding.EncodeToString(hash), // Dex's []byte field: base64 on the wire
		"username": name,
		"userID":   hex.EncodeToString(idRaw),
	}}
	created, err := s.res().Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("user %s already exists", email)
		}
		return nil, wrap(err)
	}
	return &User{Email: email, Name: name, CreatedAt: created.GetCreationTimestamp().Time}, nil
}

// SetPassword changes an account's password.
func (s *Store) SetPassword(ctx context.Context, email, password string) error {
	email, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	if err := CheckPassword(password); err != nil {
		return err
	}
	obj, err := s.res().Get(ctx, PasswordName(email), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("user %s not found", email)
		}
		return wrap(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	obj.Object["hash"] = base64.StdEncoding.EncodeToString(hash)
	if _, err := s.res().Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		return wrap(err)
	}
	return nil
}

// Delete removes an account.
func (s *Store) Delete(ctx context.Context, email string) error {
	email, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	if err := s.res().Delete(ctx, PasswordName(email), metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("user %s not found", email)
		}
		return wrap(err)
	}
	return nil
}

// wrap turns a missing CRD into ErrNotEnabled.
func wrap(err error) error {
	if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) && strings.Contains(err.Error(), "the server could not find the requested resource") {
		return ErrNotEnabled
	}
	return err
}

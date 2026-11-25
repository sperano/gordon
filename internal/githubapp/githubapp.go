// Package githubapp mints installation tokens of a GitHub App, as
// Hollingsworth's github_app_token.py does for the GitHub reactor: the
// App's JWT (RS256), signed with its private key, is exchanged for a
// one-hour installation token narrowed to one repository and the
// permissions of one kind of write. Gordon writes to GitHub as the App
// (its bot user), never with the read-only token its routing reads use,
// and a token never grants more than the write it is minted for.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// The App Secret's keys, one file each (tofu/kelos `github-app`).
const (
	AppIDFile          = "appID"
	InstallationIDFile = "installationID"
	PrivateKeyFile     = "privateKey"
)

// JWT timing (github_app_token.py): GitHub refuses a JWT issued in the
// future or valid for over ten minutes; backdating absorbs clock drift.
const (
	jwtBackdate = 60 * time.Second
	jwtLifetime = 540 * time.Second
)

// renewBefore is how long before its expiry a cached token is replaced,
// so a write never starts with a token about to lapse.
const renewBefore = 5 * time.Minute

// maxResponseBytes caps a token answer.
const maxResponseBytes = 1 << 20

// Permission sets, one per kind of write (github_reactor.py,
// review_skip.py). GitHub refuses convertPullRequestToDraft without
// Contents: write.
var (
	PullRequests = map[string]string{"pull_requests": "write"}
	Draft        = map[string]string{"pull_requests": "write", "contents": "write"}
	Checks       = map[string]string{"checks": "write"}
	// Comments posts and reads PR conversation comments and reads the
	// repository's visibility. The endpoint is /issues/{n}/comments, but on
	// a pull request an App token needs Pull requests: write; Issues:
	// write alone is refused with 403.
	Comments = map[string]string{"pull_requests": "write", "metadata": "read"}
	// Merge merges a pull request (auto_merge.MERGE_PERMISSIONS); without
	// Workflows, a PR touching .github/workflows is refused.
	Merge = map[string]string{"contents": "write", "pull_requests": "write"}
)

// App mints and caches the installation tokens of one App installation.
type App struct {
	AppID, InstallationID string
	Key                   *rsa.PrivateKey
	// BaseURL is the API base (https://api.github.com).
	BaseURL string
	HTTP    *http.Client
	Now     func() time.Time

	mu    sync.Mutex
	cache map[string]token
	// minting serializes the mints of one scope, so concurrent writers
	// on a cold or expiring entry mint one token, not one each.
	minting map[string]*sync.Mutex
	// missed, when set, runs after a writer finds no cached token and
	// before it takes the scope's lock (a test hook).
	missed func()
}

type token struct {
	value   string
	expires time.Time
}

// ErrRefused is GitHub refusing a token: a bad key, an App without the
// permission, or a repository the installation does not list. A retry
// cannot help until the owner fixes the App.
var ErrRefused = errors.New("github app token refused")

// Load reads the App's keys from dir (the mounted Secret).
func Load(dir, baseURL string, hc *http.Client) (*App, error) {
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("github app %s: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	appID, err := read(AppIDFile)
	if err != nil {
		return nil, err
	}
	inst, err := read(InstallationIDFile)
	if err != nil {
		return nil, err
	}
	pemText, err := read(PrivateKeyFile)
	if err != nil {
		return nil, err
	}
	key, err := ParseKey([]byte(pemText))
	if err != nil {
		return nil, err
	}
	if appID == "" || inst == "" {
		return nil, errors.New("github app: empty appID or installationID")
	}
	return &App{AppID: appID, InstallationID: inst, Key: key, BaseURL: strings.TrimRight(baseURL, "/"), HTTP: hc}, nil
}

// ParseKey parses a PEM RSA private key, PKCS#1 (what GitHub issues) or
// PKCS#8.
func ParseKey(text []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(text)
	if block == nil {
		return nil, errors.New("github app private key: no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("github app private key: neither PKCS#1 nor PKCS#8")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app private key: not an RSA key")
	}
	return rk, nil
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// JWT is the App's JWT, issued a minute ago and valid nine more minutes.
func (a *App) JWT() (string, error) {
	now := a.now().Unix()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"}) //nolint:errcheck // plain map
	claims, _ := json.Marshal(map[string]any{                                  //nolint:errcheck // plain map
		"iat": now - int64(jwtBackdate/time.Second), "exp": now + int64(jwtLifetime/time.Second), "iss": a.AppID,
	})
	input := b64(header) + "." + b64(claims)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("signing the github app jwt: %w", err)
	}
	return input + "." + b64(sig), nil
}

// cacheKey names a scope: the repository and its sorted permissions.
func cacheKey(repo string, perms map[string]string) string {
	keys := make([]string, 0, len(perms))
	for k, v := range perms {
		keys = append(keys, k+"="+v)
	}
	slices.Sort(keys)
	return repo + "|" + strings.Join(keys, ",")
}

// Token returns an installation token narrowed to the repository (its
// name, not owner/name, as the access_tokens API takes it) and the
// permissions, from the cache while it is valid for renewBefore more.
func (a *App) Token(ctx context.Context, repo string, perms map[string]string) (string, error) {
	key := cacheKey(repo, perms)
	if t, ok := a.cached(key); ok {
		return t, nil
	}
	if a.missed != nil {
		a.missed()
	}
	lock := a.scopeLock(key)
	lock.Lock()
	defer lock.Unlock()
	if t, ok := a.cached(key); ok { // another writer minted it meanwhile
		return t, nil
	}
	t, err := a.mint(ctx, repo, perms)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cache == nil {
		a.cache = map[string]token{}
	}
	a.cache[key] = t
	return t.value, nil
}

// cached is the scope's token while it is valid for renewBefore more.
func (a *App) cached(key string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.cache[key]
	if !ok || !a.now().Add(renewBefore).Before(t.expires) {
		return "", false
	}
	return t.value, true
}

// scopeLock is the mutex serializing the scope's mints.
func (a *App) scopeLock(key string) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.minting == nil {
		a.minting = map[string]*sync.Mutex{}
	}
	l, ok := a.minting[key]
	if !ok {
		l = &sync.Mutex{}
		a.minting[key] = l
	}
	return l
}

func (a *App) mint(ctx context.Context, repo string, perms map[string]string) (token, error) {
	jwt, err := a.JWT()
	if err != nil {
		return token{}, err
	}
	body, _ := json.Marshal(map[string]any{"repositories": []string{repo}, "permissions": perms}) //nolint:errcheck // plain map
	u := fmt.Sprintf("%s/app/installations/%s/access_tokens", a.BaseURL, a.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return token{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return token{}, fmt.Errorf("github app token: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return token{}, fmt.Errorf("github app token: %w", err)
	}
	return parseToken(resp.StatusCode, data, repo)
}

// parseToken reads the access_tokens answer; GitHub's message is kept,
// never the token.
func parseToken(status int, data []byte, repo string) (token, error) {
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
		Message   string    `json:"message"`
	}
	_ = json.Unmarshal(data, &out) //nolint:errcheck // an undecodable answer has no token
	switch {
	case status >= http.StatusInternalServerError, status == http.StatusTooManyRequests:
		return token{}, fmt.Errorf("github app token for %s: HTTP %d %s", repo, status, out.Message)
	case status != http.StatusCreated || out.Token == "" || out.ExpiresAt.IsZero():
		return token{}, fmt.Errorf("%w for %s: HTTP %d %s", ErrRefused, repo, status, out.Message)
	}
	return token{value: out.Token, expires: out.ExpiresAt}, nil
}

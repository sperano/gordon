package githubapp

import (
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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKeyBits = 2048

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func key(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, testKeyBits)
		if err != nil {
			t.Fatal(err)
		}
		testKey = k
	})
	return testKey
}

var epoch = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

type mintServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
	auth   []string
	status int
	expiry time.Time
	// held, when set, has the first mint signal arrived and wait for
	// release before answering.
	held             bool
	arrived, release chan struct{}
}

func newMint(t *testing.T) *mintServer {
	m := &mintServer{status: http.StatusCreated, expiry: epoch.Add(time.Hour)}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.hold()
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/77/access_tokens" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.bodies = append(m.bodies, body)
		m.auth = append(m.auth, r.Header.Get("Authorization"))
		w.WriteHeader(m.status)
		if m.status == http.StatusCreated {
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_" + body["repositories"].([]any)[0].(string),
				"expires_at": m.expiry.Format(time.RFC3339)})
			return
		}
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	t.Cleanup(m.Close)
	return m
}

// hold makes the first mint wait for release when the server is held.
func (m *mintServer) hold() {
	m.mu.Lock()
	first := m.held && len(m.bodies) == 0 && m.arrived != nil
	arrived := m.arrived
	if first {
		m.arrived = nil
	}
	m.mu.Unlock()
	if first {
		close(arrived)
		<-m.release
	}
}

func (m *mintServer) app(t *testing.T, now *time.Time) *App {
	return &App{AppID: "1234", InstallationID: "77", Key: key(t), BaseURL: m.URL, HTTP: m.Client(),
		Now: func() time.Time { return *now }}
}

func TestJWTIsSignedAndBackdated(t *testing.T) {
	now := epoch
	a := &App{AppID: "1234", Key: key(t), Now: func() time.Time { return now }}
	jwt, err := a.JWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if string(header) != `{"alg":"RS256","typ":"JWT"}` {
		t.Errorf("header %s", header)
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	want := `{"exp":1795262940,"iat":1795262340,"iss":"1234"}`
	if string(claims) != want {
		t.Errorf("claims %s, want %s", claims, want)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key(t).PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("signature: %v", err)
	}
}

func TestTokenIsNarrowedAndCached(t *testing.T) {
	m := newMint(t)
	now := epoch
	a := m.app(t, &now)
	ctx := context.Background()
	tok, err := a.Token(ctx, "gordon", PullRequests)
	if err != nil || tok != "ghs_gordon" {
		t.Fatalf("token %q, %v", tok, err)
	}
	if _, err := a.Token(ctx, "gordon", PullRequests); err != nil {
		t.Fatal(err)
	}
	if len(m.bodies) != 1 {
		t.Fatalf("minted %d tokens for one scope, want 1", len(m.bodies))
	}
	got, _ := json.Marshal(m.bodies[0])
	if string(got) != `{"permissions":{"pull_requests":"write"},"repositories":["gordon"]}` {
		t.Errorf("scope %s", got)
	}
	if !strings.HasPrefix(m.auth[0], "Bearer ey") {
		t.Errorf("authorization %q is not the app jwt", m.auth[0])
	}
	if _, err := a.Token(ctx, "gordon", Checks); err != nil {
		t.Fatal(err)
	}
	if len(m.bodies) != 2 {
		t.Errorf("another permission set reused the cached token")
	}
	tok, err = a.Token(ctx, "vigie", PullRequests)
	if err != nil || tok != "ghs_vigie" {
		t.Fatalf("another repository's token %q, %v; want its own", tok, err)
	}
	if len(m.bodies) != 3 {
		t.Fatalf("minted %d tokens, want a third for another repository", len(m.bodies))
	}
	if got, _ := json.Marshal(m.bodies[2]); string(got) != `{"permissions":{"pull_requests":"write"},"repositories":["vigie"]}` {
		t.Errorf("another repository's scope %s", got)
	}
}

// TestConcurrentWritersOfAScopeMintOnce: a writer that misses the cache
// while the scope's token is being minted waits for it instead of minting
// its own.
func TestConcurrentWritersOfAScopeMintOnce(t *testing.T) {
	m := newMint(t)
	m.held, m.arrived, m.release = true, make(chan struct{}), make(chan struct{})
	arrived := m.arrived
	now := epoch
	a := m.app(t, &now)
	ctx := context.Background()
	tokens := make(chan string, 2)
	call := func() {
		tok, err := a.Token(ctx, "gordon", PullRequests)
		if err != nil {
			tok = err.Error()
		}
		tokens <- tok
	}
	missed := make(chan struct{}, 2)
	a.missed = func() { missed <- struct{}{} }
	go call()
	<-missed
	<-arrived // the first mint is in flight
	go call()
	<-missed // the second writer found no token: only the lock stops its mint
	close(m.release)
	for range 2 {
		if tok := <-tokens; tok != "ghs_gordon" {
			t.Errorf("token %q, want ghs_gordon", tok)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) != 1 {
		t.Errorf("minted %d tokens for one scope, want 1", len(m.bodies))
	}
}

func TestTokenIsRenewedBeforeExpiry(t *testing.T) {
	m := newMint(t)
	now := epoch
	a := m.app(t, &now)
	if _, err := a.Token(context.Background(), "gordon", PullRequests); err != nil {
		t.Fatal(err)
	}
	now = m.expiry.Add(-renewBefore)
	if _, err := a.Token(context.Background(), "gordon", PullRequests); err != nil {
		t.Fatal(err)
	}
	if len(m.bodies) != 2 {
		t.Errorf("minted %d tokens, want a renewal within %s of expiry", len(m.bodies), renewBefore)
	}
}

func TestTokenRefusals(t *testing.T) {
	for _, tc := range []struct {
		status  int
		refused bool
	}{{http.StatusUnprocessableEntity, true}, {http.StatusForbidden, true}, {http.StatusBadGateway, false},
		{http.StatusTooManyRequests, false}} {
		m := newMint(t)
		m.status = tc.status
		now := epoch
		_, err := m.app(t, &now).Token(context.Background(), "gordon", PullRequests)
		if err == nil {
			t.Fatalf("HTTP %d: no error", tc.status)
		}
		if errors.Is(err, ErrRefused) != tc.refused {
			t.Errorf("HTTP %d: refused = %v, want %v (%v)", tc.status, !tc.refused, tc.refused, err)
		}
		if !strings.Contains(err.Error(), "Resource not accessible by integration") {
			t.Errorf("HTTP %d: GitHub's message lost: %v", tc.status, err)
		}
	}
}

func TestLoadReadsTheSecretFiles(t *testing.T) {
	dir := t.TempDir()
	der := x509.MarshalPKCS1PrivateKey(key(t))
	files := map[string]string{AppIDFile: "1234\n", InstallationIDFile: "77\n",
		PrivateKeyFile: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a, err := Load(dir, "https://api.example/", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if a.AppID != "1234" || a.InstallationID != "77" || a.BaseURL != "https://api.example" || !a.Key.Equal(key(t)) {
		t.Errorf("loaded %+v", a)
	}
	if err := os.Remove(filepath.Join(dir, InstallationIDFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "https://api.example", http.DefaultClient); err == nil || !strings.Contains(err.Error(), InstallationIDFile) {
		t.Errorf("missing installationID: %v", err)
	}
}

func TestParseKeyFormats(t *testing.T) {
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key(t))
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParseKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
	if err != nil || !k.Equal(key(t)) {
		t.Errorf("pkcs8: %v", err)
	}
	if _, err := ParseKey([]byte("not a key")); err == nil || err.Error() != "github app private key: no PEM block" {
		t.Errorf("garbage: %v", err)
	}
	if _, err := ParseKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2}})); err == nil ||
		err.Error() != "github app private key: neither PKCS#1 nor PKCS#8" {
		t.Errorf("bad der: %v", err)
	}
}

// TestPermissionSetsAreNarrow pins the scope each kind of write mints.
func TestPermissionSetsAreNarrow(t *testing.T) {
	for _, c := range []struct {
		name  string
		perms map[string]string
		want  string
	}{
		{"pull requests", PullRequests, `{"pull_requests":"write"}`},
		{"draft", Draft, `{"contents":"write","pull_requests":"write"}`},
		{"checks", Checks, `{"checks":"write"}`},
		{"comments", Comments, `{"metadata":"read","pull_requests":"write"}`},
	} {
		if got, _ := json.Marshal(c.perms); string(got) != c.want {
			t.Errorf("%s permissions %s, want %s", c.name, got, c.want)
		}
	}
}

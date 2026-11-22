package lifecycle

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sperano/gordon/internal/worker"
)

func TestWriterTokenFileIsReReadEachRequestForRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	w := &Writer{BaseURL: srv.URL, TokenFile: path, HTTP: srv.Client()}
	if _, _, err := w.LabelID(ctx, "x"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if err := os.WriteFile(path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.LabelID(ctx, "x"); err != nil {
		t.Fatalf("second read: %v", err)
	}
	if want := []string{"Bearer old", "Bearer new"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("authorization headers %v, want %v (a rotated Secret must apply without a restart)", got, want)
	}
}

func TestWriterTokenFileUnreadableIsAPlainRetryableError(t *testing.T) {
	w := &Writer{BaseURL: "http://unused.invalid", TokenFile: filepath.Join(t.TempDir(), "missing"), HTTP: http.DefaultClient}
	_, _, err := w.LabelID(ctx, "x")
	if err == nil || worker.IsPermanent(err) || worker.IsAmbiguous(err) {
		t.Fatalf("missing token file = %v, want a plain error (no request was ever sent)", err)
	}
}

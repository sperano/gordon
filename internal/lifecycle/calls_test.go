package lifecycle

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSearchesPageThrough: a label or a user past the first page of
// matches is found, and one on no page is reported missing.
func TestSearchesPageThrough(t *testing.T) {
	f := newFake(t)
	f.board()
	for i := range commentsPageSize + 1 {
		f.labels = append(f.labels, Label{ID: int64(100 + i), Title: fmt.Sprintf("x-in progress-%d", i)})
		f.users[3] = append(f.users[3], User{ID: int64(100 + i), Username: fmt.Sprintf("x-eric-%d", i)})
	}
	f.labels = append(f.labels, Label{ID: 999, Title: "progress"})
	f.users[3] = append(f.users[3], User{ID: 999, Username: "eri"})
	w := f.writer()
	if id, ok, err := w.LabelID(ctx, "progress"); err != nil || !ok || id != 999 {
		t.Errorf("LabelID past the first page: %d, %v, %v", id, ok, err)
	}
	if id, ok, err := w.ProjectUser(ctx, 3, "eri"); err != nil || !ok || id != 999 {
		t.Errorf("ProjectUser past the first page: %d, %v, %v", id, ok, err)
	}
	if id, ok, err := w.LabelID(ctx, "gres"); err != nil || ok {
		t.Errorf("LabelID of no label: %d, %v, %v", id, ok, err)
	}
	if id, ok, err := w.ProjectUser(ctx, 3, "ri"); err != nil || ok {
		t.Errorf("ProjectUser of no user: %d, %v, %v", id, ok, err)
	}
}

// TestSearchEndsWhenThePageRepeats: a server that ignores the page
// parameter serves the same full page again; the search stops there
// instead of reading maxSearchPages pages.
func TestSearchEndsWhenThePageRepeats(t *testing.T) {
	page := make([]Label, commentsPageSize)
	for i := range page {
		page[i] = Label{ID: int64(i + 1), Title: fmt.Sprintf("x-%d", i)}
	}
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		json.NewEncoder(w).Encode(page) //nolint:errcheck // test server
	}))
	defer srv.Close()
	w := &Writer{BaseURL: srv.URL, Authorization: "Bearer t", HTTP: srv.Client()}
	if id, ok, err := w.LabelID(ctx, "y"); err != nil || ok || requests != 2 {
		t.Errorf("LabelID: %d, %v, %v after %d requests, want not found after 2", id, ok, err, requests)
	}
}

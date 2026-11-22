package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/kelos/kelostest"
	"github.com/sperano/gordon/internal/route"
)

func TestTaskMessages(t *testing.T) {
	srv := kelostest.New(route.KelosNamespace)
	defer srv.Close()
	k := &kelos.Client{BaseURL: srv.URL, Namespace: route.KelosNamespace, HTTP: srv.Client()}
	srv.Put(kelos.Task{Metadata: kelos.Metadata{Name: "failed"}, Status: &kelos.Status{Phase: kelos.PhaseFailed, Message: "backoff limit"}})
	srv.Put(kelos.Task{Metadata: kelos.Metadata{Name: "fresh"}})
	read := taskMessages(k)
	for name, want := range map[string]string{"failed": "backoff limit", "fresh": "", "gone": ""} {
		if got, err := read(context.Background(), name); err != nil || got != want {
			t.Errorf("%s: %q, %v, want %q", name, got, err, want)
		}
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	k.BaseURL = down.URL
	var kerr *kelos.Error
	if got, err := read(context.Background(), "failed"); !errors.As(err, &kerr) || kerr.Status != http.StatusServiceUnavailable || got != "" {
		t.Errorf("Kelos unavailable: %q, %v, want the HTTP 503 error", got, err)
	}
}

func TestTaskLimits(t *testing.T) {
	srv := kelostest.New(route.KelosNamespace)
	defer srv.Close()
	k := &kelos.Client{BaseURL: srv.URL, Namespace: route.KelosNamespace, HTTP: srv.Client()}
	srv.Put(kelos.Task{Metadata: kelos.Metadata{Name: "stopped"}, Status: &kelos.Status{Phase: kelos.PhaseFailed,
		Results: map[string]string{kelos.ResultLimitType: "weekly"}}})
	srv.Put(kelos.Task{Metadata: kelos.Metadata{Name: "fresh"}})
	read := taskLimits(k)
	for name, want := range map[string]string{"stopped": "weekly", "fresh": "", "gone": ""} {
		if got, err := read(context.Background(), name); err != nil || got != want {
			t.Errorf("%s: %q, %v, want %q", name, got, err, want)
		}
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	k.BaseURL = down.URL
	var kerr *kelos.Error
	if got, err := read(context.Background(), "stopped"); !errors.As(err, &kerr) || kerr.Status != http.StatusServiceUnavailable || got != "" {
		t.Errorf("Kelos unavailable: %q, %v, want the HTTP 503 error", got, err)
	}
}

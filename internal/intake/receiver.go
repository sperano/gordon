// Package intake is Gordon's webhook receiver. A delivery is answered
// 2xx only once it is verified and stored: the receiver checks the
// method, the declared length against the source's cap (before reading),
// waits for a bounded slot, reads the body under the request deadline,
// verifies the source's signature, parses the delivery keys, and commits
// the row. A store failure answers 503 so the sender retries; a
// redelivery of a stored delivery answers 200 without a second row.
package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/sperano/gordon/internal/auth"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/logging"
	"github.com/sperano/gordon/internal/metrics"
	"github.com/sperano/gordon/internal/store"
)

// Path is the webhook route; {source} is a source name (vikunja, github,
// wiki-comment, image-build).
const Path = "/webhook/{source}"

// Results of the intake metric.
const (
	ResultAccepted    = "accepted"
	ResultDuplicate   = "duplicate"
	ResultIgnored     = "ignored"
	ResultRejected    = "rejected"
	ResultUnavailable = "unavailable"
)

// Reasons of a request not accepted, beyond the auth ones.
const (
	ReasonMethod      = "method-not-allowed"
	ReasonLength      = "length-required"
	ReasonTimeout     = "timeout"
	ReasonIncomplete  = "incomplete-body"
	ReasonBusy        = "busy"
	ReasonStore       = "store-unavailable"
	ReasonMalformed   = "malformed"
	ReasonUnsupported = "unsupported-event"
	ReasonUnknown     = "unknown-source"
	ReasonTooLarge    = "body-too-large"
)

var reasons = []string{"", ReasonMethod, ReasonLength, ReasonTimeout, ReasonIncomplete, ReasonBusy, ReasonStore,
	ReasonMalformed, ReasonUnsupported, ReasonUnknown, ReasonTooLarge, "signature-missing", "signature-invalid"}

// keptHeaders are the headers a stored delivery keeps: what parsing needs.
var keptHeaders = []string{event.GitHubEventHeader, event.GitHubDeliveryHeader}

// bodyDigestBytes is how much of a body digest names an unparsed
// source's delivery (wiki-comment, image-build).
const bodyDigestBytes = 16

// Inbox persists a verified delivery.
type Inbox interface {
	Accept(ctx context.Context, d store.Delivery) (id int64, dup bool, err error)
}

// Receiver answers webhook deliveries.
type Receiver struct {
	Inbox Inbox
	// Keys returns the current signing keys (re-read so a rotated key
	// file takes effect without a restart).
	Keys    func() auth.Keys
	Slots   *Slots
	Metrics *metrics.Metrics
	Log     *slog.Logger
	// Deadline bounds the whole request: waiting for a slot and reading
	// the body.
	Deadline time.Duration
}

// answer is a request's outcome.
type answer struct {
	status int
	result string
	reason string
	text   string
}

func reject(status int, reason string) answer {
	return answer{status: status, result: ResultRejected, reason: reason, text: reason}
}

// ServeHTTP handles POST /webhook/{source}.
func (rc *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	source := event.Source(r.PathValue("source"))
	log := rc.Log.With(logging.KeySource, string(source))
	a, d, id := rc.receive(r, source)
	rc.Metrics.Intake.WithLabelValues(metrics.Bound(string(source), metrics.Sources), a.result,
		metrics.Bound(a.reason, reasons)).Inc()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if a.status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", http.MethodPost)
	}
	w.WriteHeader(a.status)
	io.WriteString(w, a.text+"\n") //nolint:errcheck // the client may be gone
	attrs := []any{"result", a.result, "status", a.status, logging.KeyDelivery, d.DeliveryKey,
		logging.KeyRequest, d.RequestKey, logging.KeyKind, string(d.Kind), "bytes", len(d.Body), "peer", peer(r)}
	if a.reason != "" {
		attrs = append(attrs, "reason", a.reason)
	}
	if id != 0 {
		attrs = append(attrs, logging.KeyInbox, id)
	}
	level := slog.LevelInfo
	if a.result == ResultUnavailable {
		level = slog.LevelWarn
	}
	log.Log(r.Context(), level, "delivery", attrs...)
}

func (rc *Receiver) receive(r *http.Request, source event.Source) (answer, store.Delivery, int64) {
	maxBytes, known := auth.MaxBodyBytes(source)
	switch {
	case !known:
		return reject(http.StatusNotFound, ReasonUnknown), store.Delivery{}, 0
	case r.Method != http.MethodPost:
		return reject(http.StatusMethodNotAllowed, ReasonMethod), store.Delivery{}, 0
	case r.ContentLength < 0:
		return reject(http.StatusLengthRequired, ReasonLength), store.Delivery{}, 0
	case r.ContentLength > int64(maxBytes):
		return reject(http.StatusRequestEntityTooLarge, ReasonTooLarge), store.Delivery{}, 0
	}
	ctx, cancel := context.WithTimeout(r.Context(), rc.Deadline)
	defer cancel()
	p := peer(r)
	if !rc.Slots.Acquire(ctx, p) {
		return answer{http.StatusServiceUnavailable, ResultUnavailable, ReasonBusy, "busy"}, store.Delivery{}, 0
	}
	defer rc.Slots.Release(p)
	body, a, ok := readBody(ctx, r)
	if !ok {
		return a, store.Delivery{}, 0
	}
	return rc.store(ctx, r, source, body)
}

// readBody reads exactly the declared Content-Length; the server's read
// timeout and ctx bound the time it may take.
func readBody(ctx context.Context, r *http.Request) ([]byte, answer, bool) {
	body := make([]byte, r.ContentLength)
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(r.Body, body)
		done <- err
	}()
	select {
	case err := <-done:
		switch {
		case err == nil:
			return body, answer{}, true
		case errors.Is(err, os.ErrDeadlineExceeded):
			return nil, reject(http.StatusRequestTimeout, ReasonTimeout), false
		default:
			return nil, reject(http.StatusBadRequest, ReasonIncomplete), false
		}
	case <-ctx.Done():
		// The handler returning closes the body, which ends the read.
		return nil, reject(http.StatusRequestTimeout, ReasonTimeout), false
	}
}

func (rc *Receiver) store(ctx context.Context, r *http.Request, source event.Source, body []byte) (answer, store.Delivery, int64) {
	d := event.Delivery{Source: source, Headers: map[string]string{}, Body: body}
	for _, h := range keptHeaders {
		if v := r.Header.Get(h); v != "" {
			d.Headers[h] = v
		}
	}
	for _, h := range []string{auth.VikunjaHeader, auth.GitHubHeader, auth.DispatchHeader} {
		if v := r.Header.Get(h); v != "" {
			d.Headers[h] = v
		}
	}
	err := rc.Keys().Verify(d)
	for _, h := range []string{auth.VikunjaHeader, auth.GitHubHeader, auth.DispatchHeader} {
		delete(d.Headers, h) // verified; the signature is not kept
	}
	if err != nil {
		return reject(auth.Status(err), authReason(err)), store.Delivery{}, 0
	}
	sd, a, ok := describe(d)
	if !ok {
		return a, sd, 0
	}
	id, dup, err := rc.Inbox.Accept(ctx, sd)
	switch {
	case err != nil:
		rc.Log.Warn("store refused delivery", logging.KeyDelivery, sd.DeliveryKey, "error", err)
		return answer{http.StatusServiceUnavailable, ResultUnavailable, ReasonStore, "store unavailable"}, sd, 0
	case dup:
		return answer{http.StatusOK, ResultDuplicate, "", "duplicate"}, sd, 0
	default:
		return answer{http.StatusAccepted, ResultAccepted, "", "accepted"}, sd, id
	}
}

// describe parses a verified delivery into the row to store; false with
// the answer when it is malformed or an event nobody routes.
func describe(d event.Delivery) (store.Delivery, answer, bool) {
	sd := store.Delivery{Source: d.Source, Headers: d.Headers, Body: d.Body}
	if d.Source != event.SourceVikunja && d.Source != event.SourceGitHub {
		sum := sha256.Sum256(d.Body)
		sd.DeliveryKey = fmt.Sprintf("%s/body/%s", d.Source, hex.EncodeToString(sum[:bodyDigestBytes]))
		return sd, answer{}, true
	}
	ev, err := event.Parse(d)
	switch {
	case errors.Is(err, event.ErrUnsupported):
		return sd, answer{http.StatusAccepted, ResultIgnored, ReasonUnsupported, "ignored"}, false
	case err != nil:
		return sd, reject(http.StatusBadRequest, ReasonMalformed), false
	}
	sd.DeliveryKey, sd.RequestKey, sd.Kind, sd.Scope = ev.DeliveryKey, ev.RequestKey, ev.Kind, scope(ev)
	return sd, answer{}, true
}

// scope is the project hint the worker's per-project limit keys on.
func scope(ev event.Event) string {
	if ev.Vikunja != nil {
		return fmt.Sprintf("vikunja/project/%d", ev.Vikunja.Task.ProjectID)
	}
	return "github/" + ev.GitHub.Repository
}

func authReason(err error) string {
	switch {
	case errors.Is(err, auth.ErrUnsigned):
		return "signature-missing"
	case errors.Is(err, auth.ErrBadSignature):
		return "signature-invalid"
	case errors.Is(err, auth.ErrTooLarge):
		return ReasonTooLarge
	default:
		return ReasonUnknown
	}
}

func peer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

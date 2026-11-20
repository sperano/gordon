// Package kelostest is an in-memory Kelos Task API for tests: the
// kelos.dev/v1alpha2 tasks collection of one namespace as the Kubernetes
// API server serves it (create, get, list with a label selector and
// pages), with AlreadyExists on a taken name, an optional admission check
// and failure injection (refuse a create, or apply it and drop the
// response).
package kelostest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/sperano/gordon/internal/kelos"
)

// Admit checks a create as an admission policy would: a non-nil error
// refuses it with 422 and the error as the message.
type Admit func(kelos.Task) error

// Server is the fake API.
type Server struct {
	*httptest.Server
	Namespace string
	// Admit, when set, checks every create.
	Admit Admit
	// PageSize splits lists into pages of that many items (0: one page).
	PageSize int

	mu      sync.Mutex
	tasks   map[string]json.RawMessage
	order   []string
	creates []kelos.Task
	deletes []string
	// failCreates answers the next creates with these statuses (0: apply
	// the create, then drop the connection without answering).
	failCreates []int
	uid         int
	lastAuth    string
	// snapshots are the lists being paged, by continue-token prefix: a
	// later page reads the list as its first page saw it.
	snapshots [][]json.RawMessage
}

// DropResponse in FailCreates runs the create (admission, the name
// check, the store) and then cuts the connection whatever its outcome:
// the response-loss case.
const DropResponse = 0

// New starts a fake API for namespace.
func New(namespace string) *Server {
	s := &Server{Namespace: namespace, tasks: map[string]json.RawMessage{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// FailCreates queues statuses for the next creates, in order.
func (s *Server) FailCreates(statuses ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCreates = append(s.failCreates, statuses...)
}

// Put stores a Task as another submitter (a legacy spawner) would.
func (s *Server) Put(t kelos.Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store(t)
}

// SetPhase sets a stored Task's status phase, keeping the rest of its
// status; it panics on a Task the server does not have (a test's typo).
func (s *Server) SetPhase(name, phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var t kelos.Task
	if err := json.Unmarshal(s.tasks[name], &t); err != nil {
		panic(fmt.Sprintf("kelostest: SetPhase of task %q: %v", name, err))
	}
	if t.Status == nil {
		t.Status = &kelos.Status{}
	}
	t.Status.Phase = phase
	s.store(t)
}

// Delete removes a Task, as Kelos's TTL cleanup or an owner would.
func (s *Server) Delete(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, name)
	s.order = slices.DeleteFunc(s.order, func(n string) bool { return n == name })
}

// Task returns a stored Task.
func (s *Server) Task(name string) (kelos.Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.tasks[name]
	if !ok {
		return kelos.Task{}, false
	}
	var t kelos.Task
	_ = json.Unmarshal(raw, &t) //nolint:errcheck // stored by store()
	return t, true
}

// Raw returns a copy of a stored Task's JSON as the server keeps it.
func (s *Server) Raw(name string) json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.tasks[name])
}

// Creates returns every well-formed create request body (a Task with a
// name, in the server's namespace) that reached the server, accepted or not, in order.
func (s *Server) Creates() []kelos.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.creates)
}

// Authorization is the last request's Authorization header.
func (s *Server) Authorization() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAuth
}

func (s *Server) store(t kelos.Task) {
	if t.Metadata.UID == "" {
		s.uid++
		t.Metadata.UID = fmt.Sprintf("uid-%d", s.uid)
	}
	t.APIVersion, t.Kind, t.Metadata.Namespace = kelos.APIVersion, kelos.KindTask, s.Namespace
	raw, _ := json.Marshal(t) //nolint:errcheck // plain struct
	if _, ok := s.tasks[t.Metadata.Name]; !ok {
		s.order = append(s.order, t.Metadata.Name)
	}
	s.tasks[t.Metadata.Name] = raw
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.lastAuth = r.Header.Get("Authorization")
	s.mu.Unlock()
	prefix := fmt.Sprintf("/apis/%s/%s/namespaces/%s/tasks", kelos.Group, kelos.Version, s.Namespace)
	rest, ok := strings.CutPrefix(r.URL.Path, prefix)
	switch {
	case !ok:
		writeStatus(w, http.StatusNotFound, kelos.ReasonNotFound, "no such collection")
	case rest == "" && r.Method == http.MethodPost:
		s.create(w, r)
	case rest == "" && r.Method == http.MethodGet:
		s.list(w, r)
	case strings.HasPrefix(rest, "/") && r.Method == http.MethodGet:
		s.get(w, strings.TrimPrefix(rest, "/"))
	case strings.HasPrefix(rest, "/") && r.Method == http.MethodDelete:
		s.remove(w, r, strings.TrimPrefix(rest, "/"))
	default:
		writeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	var t kelos.Task
	if err == nil {
		err = json.Unmarshal(body, &t)
	}
	if err != nil || t.Metadata.Name == "" {
		writeStatus(w, http.StatusBadRequest, "BadRequest", "invalid Task")
		return
	}
	if ns := t.Metadata.Namespace; ns != "" && ns != s.Namespace {
		// As the API server does: the body may omit the namespace, not
		// contradict the request path's.
		writeStatus(w, http.StatusBadRequest, "BadRequest", "the namespace of the provided object does not match the namespace sent on the request")
		return
	}
	s.mu.Lock()
	s.creates = append(s.creates, t)
	fail, injected := -1, len(s.failCreates) > 0
	if injected {
		fail, s.failCreates = s.failCreates[0], s.failCreates[1:]
	}
	s.mu.Unlock()
	if injected && fail != DropResponse {
		writeStatus(w, fail, reasonFor(fail), "injected failure")
		return
	}
	code, reason, msg, raw := s.apply(t)
	switch {
	case injected: // DropResponse: whatever the outcome, the client never hears it
		hijack(w)
	case code != http.StatusCreated:
		writeStatus(w, code, reason, msg)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write(raw) //nolint:errcheck // test server
	}
}

// apply runs admission and the storage create of t: the status code, and
// the reason and message of a refusal or the stored Task of a create.
func (s *Server) apply(t kelos.Task) (code int, reason, msg string, raw json.RawMessage) {
	s.mu.Lock()
	admit := s.Admit
	s.mu.Unlock()
	// Validating admission runs before the storage create, as in the API
	// server, and outside the lock: Admit may read the server.
	if admit != nil {
		if err := admit(t); err != nil {
			return http.StatusUnprocessableEntity, reasonFor(http.StatusUnprocessableEntity), err.Error(), nil
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.tasks[t.Metadata.Name]; taken {
		return http.StatusConflict, kelos.ReasonAlreadyExists, "tasks.kelos.dev \"" + t.Metadata.Name + "\" already exists", nil
	}
	// The API server assigns a create's UID, ignoring any the client sent.
	t.Metadata.UID = ""
	s.store(t)
	return http.StatusCreated, "", "", s.tasks[t.Metadata.Name]
}

// reasons are the Status reasons the API server gives a create's
// failures, by code.
var reasons = map[int]string{
	http.StatusBadRequest:          "BadRequest",
	http.StatusUnauthorized:        "Unauthorized",
	http.StatusForbidden:           "Forbidden",
	http.StatusNotFound:            kelos.ReasonNotFound,
	http.StatusConflict:            kelos.ReasonAlreadyExists,
	http.StatusUnprocessableEntity: "Invalid",
	http.StatusTooManyRequests:     "TooManyRequests",
	http.StatusServiceUnavailable:  "ServiceUnavailable",
	http.StatusGatewayTimeout:      "Timeout",
}

// reasonFor is the reason of a failure of code: an injected 409 is
// AlreadyExists and a 422 Invalid, as the API server answers them.
func reasonFor(code int) string {
	if r, ok := reasons[code]; ok {
		return r
	}
	return "InternalError"
}

// remove answers a DELETE as the API server does: a Success Status, or
// NotFound, or a Conflict when the body's preconditions.uid is not the
// stored Task's.
func (s *Server) remove(w http.ResponseWriter, r *http.Request, name string) {
	var opts struct {
		Preconditions struct {
			UID string `json:"uid"`
		} `json:"preconditions"`
	}
	if body, err := io.ReadAll(r.Body); err != nil || len(body) > 0 && json.Unmarshal(body, &opts) != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", "invalid DeleteOptions")
		return
	}
	s.mu.Lock()
	raw, ok := s.tasks[name]
	var stored kelos.Task
	_ = json.Unmarshal(raw, &stored) //nolint:errcheck // stored by store()
	uid := opts.Preconditions.UID
	conflict := ok && uid != "" && uid != stored.Metadata.UID
	if ok && !conflict {
		delete(s.tasks, name)
		s.order = slices.DeleteFunc(s.order, func(n string) bool { return n == name })
		s.deletes = append(s.deletes, name)
	}
	s.mu.Unlock()
	switch {
	case !ok:
		writeStatus(w, http.StatusNotFound, kelos.ReasonNotFound, "tasks.kelos.dev \""+name+"\" not found")
	case conflict:
		writeStatus(w, http.StatusConflict, ReasonConflict, "Precondition failed: UID in precondition: "+uid+
			", UID in object meta: "+stored.Metadata.UID)
	default:
		writeJSONStatus(w, http.StatusOK, statusSuccess, "", "deleted")
	}
}

// ReasonConflict is the Status reason of a refused delete precondition.
const ReasonConflict = "Conflict"

// Status values of a Kubernetes Status.
const (
	statusSuccess = "Success"
	statusFailure = "Failure"
)

// Deletes lists the Tasks deleted through the API, in order.
func (s *Server) Deletes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.deletes)
}

func (s *Server) get(w http.ResponseWriter, name string) {
	s.mu.Lock()
	raw, ok := s.tasks[name]
	s.mu.Unlock()
	if !ok {
		writeStatus(w, http.StatusNotFound, kelos.ReasonNotFound, "tasks.kelos.dev \""+name+"\" not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(raw) //nolint:errcheck // test server
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	sel, err := parseSelector(r.URL.Query().Get("labelSelector"))
	if err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	items, snap, start, err := s.page(r, sel)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	end, next := len(items), ""
	if s.PageSize > 0 && start+s.PageSize < len(items) {
		end = start + s.PageSize
		next = fmt.Sprintf("%d/%d", snap, end)
	}
	page := struct {
		Metadata map[string]string `json:"metadata"`
		Items    []json.RawMessage `json:"items"`
	}{Metadata: map[string]string{"continue": next}, Items: items[start:end]}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(page) //nolint:errcheck // test server
}

// page returns the list a request pages through, its snapshot and the
// offset of the page: the matching Tasks now on a first page, the snapshot its first
// page took on a later one (a continue token "<snapshot>/<offset>"), as
// the API server serves a list's pages from one resourceVersion. A token
// this server did not hand out is an error, as the API server refuses an
// invalid continue key.
func (s *Server) page(r *http.Request, sel selector) (items []json.RawMessage, snap, start int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token := r.URL.Query().Get("continue"); token != "" {
		return s.resume(token)
	}
	for _, name := range s.order {
		var t kelos.Task
		if json.Unmarshal(s.tasks[name], &t) == nil && sel.matches(t.Metadata.Labels) {
			items = append(items, s.tasks[name])
		}
	}
	s.snapshots = append(s.snapshots, items)
	return items, len(s.snapshots) - 1, 0, nil
}

// resume reads a continue token "<snapshot>/<offset>": the snapshot it
// names and the offset of the page, which must lie inside that snapshot.
// The caller holds s.mu.
func (s *Server) resume(token string) (items []json.RawMessage, snap, start int, err error) {
	bad := fmt.Errorf("continue key %q is not valid", token)
	a, b, ok := strings.Cut(token, "/")
	if !ok {
		return nil, 0, 0, bad
	}
	if snap, err = strconv.Atoi(a); err != nil || snap < 0 || snap >= len(s.snapshots) {
		return nil, 0, 0, bad
	}
	items = s.snapshots[snap]
	if start, err = strconv.Atoi(b); err != nil || start < 0 || start > len(items) {
		return nil, 0, 0, bad
	}
	return items, snap, start, nil
}

// requirement is one k=v term of a label selector.
// requirement is key=value, or with absent the key's absence (!key).
type requirement struct {
	key, value string
	absent     bool
}

// selector is every requirement of a label selector, repeated keys
// included: as in the API server, a Task must meet them all.
type selector []requirement

// parseSelector reads equality selectors (k=v,k2=v2) and absence
// selectors (!k), all the client sends (the drag-cancel's listing uses
// !k); any other syntax (k!=v, k==v, k in (...), a bare k) is an error,
// so a test cannot pass on a selector the fake misread.
func parseSelector(s string) (selector, error) {
	var out selector
	if s == "" {
		return out, nil
	}
	for part := range strings.SplitSeq(s, ",") {
		if k, ok := strings.CutPrefix(part, "!"); ok && k != "" && !strings.ContainsAny(k, "!=() ") {
			out = append(out, requirement{key: k, absent: true})
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || k == "" || strings.ContainsAny(k, "!=() ") || strings.ContainsAny(v, "=!() ") {
			return nil, fmt.Errorf("unsupported label selector %q", part)
		}
		out = append(out, requirement{key: k, value: v})
	}
	return out, nil
}

func (s selector) matches(labels map[string]string) bool {
	for _, r := range s {
		got, ok := labels[r.key]
		if r.absent && ok || !r.absent && (!ok || got != r.value) {
			return false
		}
	}
	return true
}

// writeStatus answers a failure Status.
func writeStatus(w http.ResponseWriter, code int, reason, msg string) {
	writeJSONStatus(w, code, statusFailure, reason, msg)
}

func writeJSONStatus(w http.ResponseWriter, code int, status, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // test server
		"kind": "Status", "apiVersion": "v1", "status": status, "reason": reason, "message": msg, "code": code,
	})
}

func hijack(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			conn.Close()
		}
	}
}

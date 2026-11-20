// Package kelostest is an in-memory Kelos Task API for tests: the
// kelos.dev/v1alpha2 tasks collection of one namespace as the Kubernetes
// API server serves it (create, get, list with a label selector and
// pages), with AlreadyExists on a taken name, an optional admission check
// and failure injection (refuse a create, or apply it and drop the
// response).
package kelostest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
	// failCreates answers the next creates with these statuses (0: apply
	// the create, then drop the connection without answering).
	failCreates []int
	uid         int
	lastAuth    string
}

// DropResponse in FailCreates applies the create and then cuts the
// connection: the response-loss case.
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

// SetPhase sets a stored Task's status phase.
func (s *Server) SetPhase(name, phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var t kelos.Task
	if json.Unmarshal(s.tasks[name], &t) != nil {
		return
	}
	t.Status = &kelos.Status{Phase: phase}
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

// Raw returns a stored Task's JSON as the server keeps it.
func (s *Server) Raw(name string) json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tasks[name]
}

// Creates returns every well-formed create request body (a Task with a
// name) that reached the server, accepted or not, in order.
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
	s.mu.Lock()
	s.creates = append(s.creates, t)
	fail, injected := -1, len(s.failCreates) > 0
	if injected {
		fail, s.failCreates = s.failCreates[0], s.failCreates[1:]
	}
	if injected && fail != DropResponse {
		s.mu.Unlock()
		writeStatus(w, fail, "InternalError", "injected failure")
		return
	}
	if _, taken := s.tasks[t.Metadata.Name]; taken {
		s.mu.Unlock()
		writeStatus(w, http.StatusConflict, kelos.ReasonAlreadyExists, "tasks.kelos.dev \""+t.Metadata.Name+"\" already exists")
		return
	}
	if s.Admit != nil {
		if err := s.Admit(t); err != nil {
			s.mu.Unlock()
			writeStatus(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
			return
		}
	}
	s.store(t)
	raw := s.tasks[t.Metadata.Name]
	s.mu.Unlock()
	if injected { // DropResponse: applied, but the client never hears it
		hijack(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(raw) //nolint:errcheck // test server
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
	selector := parseSelector(r.URL.Query().Get("labelSelector"))
	s.mu.Lock()
	var items []json.RawMessage
	for _, name := range s.order {
		var t kelos.Task
		if json.Unmarshal(s.tasks[name], &t) == nil && selector.matches(t.Metadata.Labels) {
			items = append(items, s.tasks[name])
		}
	}
	s.mu.Unlock()
	start := 0
	fmt.Sscan(r.URL.Query().Get("continue"), &start) //nolint:errcheck // "" is the first page
	end, next := len(items), ""
	if s.PageSize > 0 && start+s.PageSize < len(items) {
		end, next = start+s.PageSize, fmt.Sprint(start+s.PageSize)
	}
	page := struct {
		Metadata map[string]string `json:"metadata"`
		Items    []json.RawMessage `json:"items"`
	}{Metadata: map[string]string{"continue": next}, Items: items[min(start, len(items)):end]}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(page) //nolint:errcheck // test server
}

type selector map[string]string

// parseSelector reads equality selectors (k=v,k2=v2), all the client uses.
func parseSelector(s string) selector {
	out := selector{}
	for part := range strings.SplitSeq(s, ",") {
		if k, v, ok := strings.Cut(part, "="); ok {
			out[k] = v
		}
	}
	return out
}

func (s selector) matches(labels map[string]string) bool {
	for k, v := range s {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func writeStatus(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // test server
		"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": reason, "message": msg, "code": code,
	})
}

func hijack(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			conn.Close()
		}
	}
}

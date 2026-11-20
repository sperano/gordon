package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/store"
)

// fakeVikunja is an in-memory Vikunja API: the routes the writer calls,
// with Vikunja's methods (PUT creates). failNext makes the next request
// to a path pattern apply its write, then answer 500: a write whose
// outcome the caller cannot know.
type fakeVikunja struct {
	*httptest.Server
	mu        sync.Mutex
	tasks     map[int64]*Task
	labels    []Label
	views     map[int64][]View
	buckets   map[int64][]Bucket // by view
	users     map[int64][]User   // by project
	comments  map[int64][]Comment
	reactions map[int64]map[string][]User
	nextID    int64
	writes    []string
	failNext  map[string]bool
}

func newFake(t *testing.T) *fakeVikunja {
	f := &fakeVikunja{tasks: map[int64]*Task{}, views: map[int64][]View{}, buckets: map[int64][]Bucket{},
		users: map[int64][]User{}, comments: map[int64][]Comment{}, reactions: map[int64]map[string][]User{},
		nextID: 1000, failNext: map[string]bool{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

const gordon = "gordon"

func (f *fakeVikunja) writer() *Writer {
	return &Writer{BaseURL: f.URL, Authorization: "Bearer t", HTTP: f.Client()}
}

type fakeRoute struct {
	method string
	re     *regexp.Regexp
	fn     func(f *fakeVikunja, w http.ResponseWriter, r *http.Request, ids []int64)
}

var routes = []fakeRoute{
	{"GET", regexp.MustCompile(`^/tasks/(\d+)$`), (*fakeVikunja).getTask},
	{"POST", regexp.MustCompile(`^/tasks/(\d+)$`), (*fakeVikunja).updateTask},
	{"GET", regexp.MustCompile(`^/tasks/(\d+)/comments$`), (*fakeVikunja).listComments},
	{"PUT", regexp.MustCompile(`^/tasks/(\d+)/comments$`), (*fakeVikunja).addComment},
	{"POST", regexp.MustCompile(`^/tasks/(\d+)/comments/(\d+)$`), (*fakeVikunja).editComment},
	{"PUT", regexp.MustCompile(`^/tasks/(\d+)/labels$`), (*fakeVikunja).addLabel},
	{"DELETE", regexp.MustCompile(`^/tasks/(\d+)/labels/(\d+)$`), (*fakeVikunja).removeLabel},
	{"PUT", regexp.MustCompile(`^/tasks/(\d+)/assignees$`), (*fakeVikunja).assign},
	{"GET", regexp.MustCompile(`^/labels$`), (*fakeVikunja).listLabels},
	{"GET", regexp.MustCompile(`^/projects/(\d+)/views$`), (*fakeVikunja).listViews},
	{"GET", regexp.MustCompile(`^/projects/(\d+)/views/(\d+)/buckets$`), (*fakeVikunja).listBuckets},
	{"POST", regexp.MustCompile(`^/projects/(\d+)/views/(\d+)/buckets/(\d+)/tasks$`), (*fakeVikunja).move},
	{"GET", regexp.MustCompile(`^/projects/(\d+)/projectusers$`), (*fakeVikunja).projectUsers},
	{"GET", regexp.MustCompile(`^/comments/(\d+)/reactions$`), (*fakeVikunja).listReactions},
	{"PUT", regexp.MustCompile(`^/comments/(\d+)/reactions$`), (*fakeVikunja).react},
}

func (f *fakeVikunja) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rt := range routes {
		m := rt.re.FindStringSubmatch(r.URL.Path)
		if m == nil || rt.method != r.Method {
			continue
		}
		var ids []int64
		for _, s := range m[1:] {
			n, _ := strconv.ParseInt(s, 10, 64)
			ids = append(ids, n)
		}
		if r.Method != http.MethodGet {
			f.writes = append(f.writes, r.Method+" "+r.URL.Path)
		}
		key := r.Method + " " + rt.re.String()
		rt.fn(f, w, r, ids)
		if f.failNext[key] {
			delete(f.failNext, key)
			// The write applied; the answer is lost.
			panic(http.ErrAbortHandler)
		}
		return
	}
	http.Error(w, "no route "+r.Method+" "+r.URL.Path, http.StatusNotFound)
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck // test server
}

func (f *fakeVikunja) task(w http.ResponseWriter, id int64) *Task {
	t, ok := f.tasks[id]
	if !ok {
		http.Error(w, `{"code":4002}`, http.StatusNotFound)
	}
	return t
}

func (f *fakeVikunja) getTask(w http.ResponseWriter, _ *http.Request, ids []int64) {
	if t := f.task(w, ids[0]); t != nil {
		reply(w, t)
	}
}

func (f *fakeVikunja) updateTask(w http.ResponseWriter, r *http.Request, ids []int64) {
	t := f.task(w, ids[0])
	if t == nil {
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test server
	if d, ok := body["done"].(bool); ok {
		t.Done = d
	}
	reply(w, t)
}

func (f *fakeVikunja) listComments(w http.ResponseWriter, r *http.Request, ids []int64) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	all := f.comments[ids[0]]
	from := min(max(page-1, 0)*commentsPageSize, len(all))
	reply(w, all[from:min(from+commentsPageSize, len(all))])
}

func (f *fakeVikunja) addComment(w http.ResponseWriter, r *http.Request, ids []int64) {
	var body struct{ Comment string }
	json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test server
	f.nextID++
	c := Comment{ID: f.nextID, Comment: body.Comment, Author: User{ID: 80, Username: gordon}}
	f.comments[ids[0]] = append(f.comments[ids[0]], c)
	reply(w, c)
}

func (f *fakeVikunja) editComment(w http.ResponseWriter, r *http.Request, ids []int64) {
	var body struct{ Comment string }
	json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test server
	for i, c := range f.comments[ids[0]] {
		if c.ID == ids[1] {
			f.comments[ids[0]][i].Comment = body.Comment
			reply(w, f.comments[ids[0]][i])
			return
		}
	}
	http.Error(w, "no comment", http.StatusNotFound)
}

func (f *fakeVikunja) addLabel(w http.ResponseWriter, r *http.Request, ids []int64) {
	var body struct {
		LabelID int64 `json:"label_id"`
	}
	json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test server
	t := f.task(w, ids[0])
	i := slices.IndexFunc(f.labels, func(l Label) bool { return l.ID == body.LabelID })
	if t == nil || i < 0 {
		return
	}
	t.Labels = append(t.Labels, f.labels[i])
	reply(w, f.labels[i])
}

func (f *fakeVikunja) removeLabel(w http.ResponseWriter, _ *http.Request, ids []int64) {
	if t := f.task(w, ids[0]); t != nil {
		t.Labels = slices.DeleteFunc(t.Labels, func(l Label) bool { return l.ID == ids[1] })
		reply(w, map[string]string{})
	}
}

func (f *fakeVikunja) assign(w http.ResponseWriter, r *http.Request, ids []int64) {
	var body struct {
		UserID int64 `json:"user_id"`
	}
	json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test server
	t := f.task(w, ids[0])
	if t == nil {
		return
	}
	for _, u := range f.users[t.ProjectID] {
		if u.ID == body.UserID {
			t.Assignees = append(t.Assignees, u)
		}
	}
	reply(w, map[string]string{})
}

func (f *fakeVikunja) listLabels(w http.ResponseWriter, r *http.Request, _ []int64) {
	s := r.URL.Query().Get("s")
	reply(w, slices.DeleteFunc(slices.Clone(f.labels), func(l Label) bool { return !regexp.MustCompile(regexp.QuoteMeta(s)).MatchString(l.Title) }))
}

func (f *fakeVikunja) listViews(w http.ResponseWriter, _ *http.Request, ids []int64) {
	reply(w, f.views[ids[0]])
}

func (f *fakeVikunja) listBuckets(w http.ResponseWriter, _ *http.Request, ids []int64) {
	reply(w, f.buckets[ids[1]])
}

func (f *fakeVikunja) move(w http.ResponseWriter, r *http.Request, ids []int64) {
	var body struct {
		TaskID int64 `json:"task_id"`
	}
	json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test server
	t := f.task(w, body.TaskID)
	if t == nil {
		return
	}
	t.Buckets = slices.DeleteFunc(t.Buckets, func(b Bucket) bool { return b.ViewID == ids[1] })
	for _, b := range f.buckets[ids[1]] {
		if b.ID == ids[2] {
			t.Buckets = append(t.Buckets, b)
		}
	}
	reply(w, map[string]string{})
}

func (f *fakeVikunja) projectUsers(w http.ResponseWriter, _ *http.Request, ids []int64) {
	reply(w, f.users[ids[0]])
}

func (f *fakeVikunja) listReactions(w http.ResponseWriter, _ *http.Request, ids []int64) {
	reply(w, f.reactions[ids[0]])
}

func (f *fakeVikunja) react(w http.ResponseWriter, r *http.Request, ids []int64) {
	var body struct{ Value string }
	json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck // test server
	if f.reactions[ids[0]] == nil {
		f.reactions[ids[0]] = map[string][]User{}
	}
	f.reactions[ids[0]][body.Value] = append(f.reactions[ids[0]][body.Value], User{ID: 80, Username: gordon})
	reply(w, map[string]string{})
}

// board gives project 3 two manual Kanban views (To-Do/Doing/Review in
// each) and a list view, and the labels Gordon sees.
func (f *fakeVikunja) board() {
	f.views[3] = []View{{ID: 1, Kind: "kanban", BucketMode: "manual"}, {ID: 2, Kind: "list"},
		{ID: 3, Kind: "kanban", BucketMode: "manual"}}
	for _, v := range []int64{1, 3} {
		for i, title := range []string{"To-Do", "Doing", "Review"} {
			f.buckets[v] = append(f.buckets[v], Bucket{ID: v*10 + int64(i), Title: title, ViewID: v})
		}
	}
	f.labels = []Label{{ID: 1, Title: "in progress"}, {ID: 2, Title: "action-needed"}, {ID: 3, Title: "agent-error"},
		{ID: 4, Title: "chain"}, {ID: 5, Title: "auto-merge", CreatedBy: User{Username: "eric"}},
		{ID: 6, Title: "auto-merge", CreatedBy: User{Username: "mallory"}}}
	f.users[3] = []User{{ID: 7, Username: "claude-opus"}, {ID: 8, Username: "eric"}}
}

func (f *fakeVikunja) put(t Task) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[t.ID] = &t
}

func (f *fakeVikunja) get(id int64) Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.tasks[id]
}

func (f *fakeVikunja) inBucket(id int64, view int64) string {
	for _, b := range f.get(id).Buckets {
		if b.ViewID == view {
			return b.Title
		}
	}
	return ""
}

// runs serves RunInfo from a map.
type runs map[string]store.RunInfo

func (r runs) RunInfo(_ context.Context, name string) (store.RunInfo, error) {
	if i, ok := r[name]; ok {
		return i, nil
	}
	return store.RunInfo{}, fmt.Errorf("%w: %s", store.ErrUnknownRun, name)
}

// noLock is a Locker for tests that run one executor.
type noLock struct{}

func (noLock) WithTargetLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}

func executor(f *fakeVikunja, rs runs) *Executor {
	p := profile.Defaults().Policy
	p.VikunjaWebURL = "https://vikunja.example"
	return &Executor{W: f.writer(), Policy: p, Agents: []string{"claude-opus", "gpt-sol"}, Runs: rs, Lock: noLock{},
		Namespace: "kelos-agents"}
}

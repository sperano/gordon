package upstream

import (
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sperano/gordon/internal/event"
)

func TestVikunjaTaskRelations(t *testing.T) {
	c, _ := client(t, ProviderVikunja, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":4,"project_id":3,"index":12,"related_tasks":{
			"blocked":[{"id":9,"project_id":136,"title":"B","done":false}],
			"blocking":[{"id":10,"project_id":3,"done":true}],
			"subtask":[{"id":11,"project_id":3,"index":1,"done":false}],
			"parenttask":[{"id":12,"project_id":3,"done":false}],
			"related":[{"id":13}]}}`)
	})
	got, err := Vikunja{c}.Task(ctx, 4)
	want := event.Relations{
		BlockedBy: []event.Related{{ID: 9, ProjectID: 136, Title: "B"}},
		Blocking:  []event.Related{{ID: 10, ProjectID: 3, Done: true}},
		Subtasks:  []event.Related{{ID: 11, ProjectID: 3, Index: 1}},
		Parents:   []event.Related{{ID: 12, ProjectID: 3}},
	}
	if err != nil || got.Index != 12 || !reflect.DeepEqual(got.Relations, want) {
		t.Fatalf("Task = %+v, %v; want relations %+v", got, err, want)
	}
	if open := got.OpenBlockers(); len(open) != 1 || open[0].ID != 9 {
		t.Fatalf("open blockers %+v", open)
	}
}

func TestVikunjaTasksListing(t *testing.T) {
	var queries []string
	c, _ := client(t, ProviderVikunja, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		switch r.URL.Query().Get("page") {
		case "1":
			var items []string
			for i := range TaskPageSize {
				items = append(items, fmt.Sprintf(`{"id":%d,"project_id":3}`, i+1))
			}
			fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
		case "2":
			fmt.Fprint(w, `[{"id":51,"project_id":3,"updated":"2026-11-20T10:00:00Z","comment_count":2,
				"comments":[{"id":7,"comment":"<p>/gordon x</p>","author":{"username":"eric"},
				"created":"2026-11-20T09:00:00Z","reactions":{"👎":[{"username":"eric"}]}}]}]`)
		case "3":
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("page %s read", r.URL.Query().Get("page"))
		}
	})
	got, err := Vikunja{c}.Tasks(ctx, "done = false && project in 3", true)
	if err != nil || len(got) != TaskPageSize+1 {
		t.Fatalf("Tasks = %d tasks, %v", len(got), err)
	}
	last := got[TaskPageSize]
	if last.CommentCount != 2 || len(last.Comments) != 1 || last.Comments[0].Author != "eric" ||
		!last.Comments[0].ReactedBy("👎", "eric") || !last.Updated.Equal(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("listed task %+v", last)
	}
	if !strings.Contains(queries[0], "expand=buckets&expand=comments&expand=comment_count") ||
		!strings.Contains(queries[0], "filter=done+%3D+false") {
		t.Fatalf("query %q", queries[0])
	}
}

// pagedTasks serves pages of size tasks each, numbered from 1, up to
// pages pages, then empty pages; it counts the pages read.
func pagedTasks(t *testing.T, size, pages int, read *int) Vikunja {
	c, _ := client(t, ProviderVikunja, func(w http.ResponseWriter, r *http.Request) {
		*read++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		var items []string
		for i := range size {
			if page <= pages {
				items = append(items, fmt.Sprintf(`{"id":%d,"project_id":3}`, (page-1)*size+i+1))
			}
		}
		fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
	})
	return Vikunja{c}
}

func TestVikunjaTasksServerPageCapBelowOurs(t *testing.T) {
	const serverPage, serverPages = 10, 3
	read := 0
	got, err := pagedTasks(t, serverPage, serverPages, &read).Tasks(ctx, "done = false", false)
	if err != nil || len(got) != serverPage*serverPages || read != serverPages+1 {
		t.Fatalf("Tasks = %d tasks in %d reads, %v; want %d in %d", len(got), read, err, serverPage*serverPages, serverPages+1)
	}
}

func TestVikunjaTasksExactlyAtTheCap(t *testing.T) {
	read := 0
	got, err := pagedTasks(t, TaskPageSize, maxTaskPages, &read).Tasks(ctx, "done = false", false)
	if err != nil || len(got) != TaskPageSize*maxTaskPages {
		t.Fatalf("Tasks = %d tasks, %v; want %d", len(got), err, TaskPageSize*maxTaskPages)
	}
}

func TestVikunjaTasksOverTheCap(t *testing.T) {
	read := 0
	_, err := pagedTasks(t, TaskPageSize, maxTaskPages+1, &read).Tasks(ctx, "done = false", false)
	if err == nil || err.Error() != fmt.Sprintf("vikunja: invalid: more than %d task pages", maxTaskPages) {
		t.Fatalf("Tasks over the cap: %v", err)
	}
}

func TestVikunjaTaskCommentsAndLabel(t *testing.T) {
	c, _ := client(t, ProviderVikunja, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/labels":
			fmt.Fprint(w, `[{"id":1,"title":"in progress (old)"},{"id":2,"title":"in progress"}]`)
		case r.URL.Query().Get("page") == "1":
			fmt.Fprint(w, `[{"id":5,"comment":"hi","author":{"username":"gordon"},"created":"2026-11-20T09:00:00Z"}]`)
		default:
			fmt.Fprint(w, `[]`)
		}
	})
	cs, err := Vikunja{c}.TaskComments(ctx, 4)
	if err != nil || len(cs) != 1 || cs[0].Author != "gordon" || cs[0].Body != "hi" {
		t.Fatalf("TaskComments = %+v, %v", cs, err)
	}
	id, ok, err := Vikunja{c}.LabelID(ctx, "in progress")
	if err != nil || !ok || id != 2 {
		t.Fatalf("LabelID = %d %v %v, want the exact title's 2", id, ok, err)
	}
	if _, ok, _ := (Vikunja{c}).LabelID(ctx, "progress"); ok {
		t.Fatal("a partial title matched")
	}
}

// pagedLabels serves pages full of "in progress (n)" labels up to pages,
// then the exact "in progress" label alone on the next page.
func pagedLabels(t *testing.T, pages int, read *int) Vikunja {
	c, _ := client(t, ProviderVikunja, func(w http.ResponseWriter, r *http.Request) {
		*read++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page > pages {
			fmt.Fprint(w, `[{"id":9999,"title":"in progress"}]`)
			return
		}
		items := make([]string, TaskPageSize)
		for i := range items {
			id := (page-1)*TaskPageSize + i + 1
			items[i] = fmt.Sprintf(`{"id":%d,"title":"in progress (%d)"}`, id, id)
		}
		fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
	})
	return Vikunja{c}
}

func TestVikunjaLabelIDPastTheFirstPage(t *testing.T) {
	read := 0
	id, ok, err := pagedLabels(t, 2, &read).LabelID(ctx, "in progress")
	if err != nil || !ok || id != 9999 || read != 3 {
		t.Fatalf("LabelID = %d %v %v in %d reads, want 9999 on the third page", id, ok, err, read)
	}
}

func TestVikunjaLabelIDRefusesMoreThanTheCap(t *testing.T) {
	read := 0
	_, ok, err := pagedLabels(t, maxTaskPages, &read).LabelID(ctx, "in progress")
	want := fmt.Sprintf(`vikunja: invalid: labels matching "in progress": more than %d pages`, maxTaskPages)
	if ok || err == nil || err.Error() != want || read != maxTaskPages {
		t.Fatalf("LabelID = %v, %v after %d reads; want %q after %d", ok, err, read, want, maxTaskPages)
	}
}

// TestVikunjaLabelIDStopsOnARepeatedPage: a server ignoring the page
// parameter serves the same full page again; the second read adds no new
// label, so LabelID reports none instead of reading up to the cap.
func TestVikunjaLabelIDStopsOnARepeatedPage(t *testing.T) {
	read := 0
	c, _ := client(t, ProviderVikunja, func(w http.ResponseWriter, _ *http.Request) {
		read++
		items := make([]string, TaskPageSize)
		for i := range items {
			items[i] = fmt.Sprintf(`{"id":%d,"title":"in progress (%d)"}`, i+1, i+1)
		}
		fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
	})
	const wantReads = 2
	id, ok, err := Vikunja{c}.LabelID(ctx, "in progress")
	if err != nil || ok || id != 0 || read != wantReads {
		t.Fatalf("LabelID = %d %v %v in %d reads, want none after %d", id, ok, err, read, wantReads)
	}
}

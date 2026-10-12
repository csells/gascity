package api

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// lateWriteStore models a write that commits after a list handler has read
// its snapshot but before the handler finishes. The first List returns the
// pre-write rows, then creates a bead and records its event, exactly as a
// concurrent writer would.
type lateWriteStore struct {
	beads.Store
	rec  events.Recorder
	once sync.Once
	late string
}

func (s *lateWriteStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(q)
	s.once.Do(func() {
		b, createErr := s.Create(beads.Bead{Title: "late write"})
		if createErr != nil {
			panic(createErr)
		}
		s.late = b.ID
		s.rec.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: b.ID})
	})
	return rows, err
}

func responseIndex(t *testing.T, rec *httptest.ResponseRecorder) uint64 {
	t.Helper()
	raw := rec.Header().Get("X-GC-Index")
	if raw == "" {
		t.Fatal("response missing X-GC-Index")
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		t.Fatalf("X-GC-Index %q: %v", raw, err)
	}
	return n
}

func decodeBeadList(t *testing.T, rec *httptest.ResponseRecorder) boundedListBody {
	t.Helper()
	var body boundedListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	return body
}

func bodyHasBead(items []beads.Bead, id string) bool {
	for _, b := range items {
		if b.ID == id {
			return true
		}
	}
	return false
}

// TestListIndexDoesNotSkipChangesMissingFromSnapshot pins the X-GC-Index
// contract that resume and blocking queries rely on: a client that resumes
// the event stream after the returned index, or blocks on it, must observe
// every change the returned body does not already reflect. Reading the index
// after building the snapshot lets a concurrent write land in between; its
// event is then at or below the index, so a resuming client never sees it.
func TestListIndexDoesNotSkipChangesMissingFromSnapshot(t *testing.T) {
	fs := newFakeState(t)
	ep := fs.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: "seed"})
	store := &lateWriteStore{Store: fs.stores["myrig"], rec: ep}
	fs.stores["myrig"] = store

	h := newTestCityHandler(t, fs)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(fs, "/beads"), nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	body := decodeBeadList(t, rec)
	if store.late == "" {
		t.Fatal("late write did not run; the list path no longer reads this store")
	}
	if bodyHasBead(body.Items, store.late) {
		t.Fatal("snapshot already contains the late write; the test no longer models the race")
	}
	lateSeq := ep.Events[len(ep.Events)-1].Seq
	if idx := responseIndex(t, rec); idx >= lateSeq {
		t.Fatalf("X-GC-Index = %d, want < %d: the body omits bead %s, but resuming after the index skips its event", idx, lateSeq, store.late)
	}
}

// TestCachedListIndexDoesNotSkipChangesMissingFromBody covers the response
// cache: a cached body must be served with the index at which it was built,
// not the current head, or changes since the build are skipped on resume.
func TestCachedListIndexDoesNotSkipChangesMissingFromBody(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Hour
	t.Cleanup(func() { timeBucketResponseCacheTTL = oldTTL })
	fs := newFakeState(t)
	ep := fs.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: "seed"})
	h := newTestCityHandler(t, fs)

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest("GET", cityURL(fs, "/beads")+"?all=true", nil))
	if first.Code != 200 {
		t.Fatalf("first status = %d (body=%q)", first.Code, first.Body.String())
	}

	b, err := fs.stores["myrig"].Create(beads.Bead{Title: "after cache fill"})
	if err != nil {
		t.Fatal(err)
	}
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: b.ID})
	changeSeq := ep.Events[len(ep.Events)-1].Seq

	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest("GET", cityURL(fs, "/beads")+"?all=true", nil))
	if second.Code != 200 {
		t.Fatalf("second status = %d (body=%q)", second.Code, second.Body.String())
	}
	body := decodeBeadList(t, second)
	if bodyHasBead(body.Items, b.ID) {
		t.Fatal("second response was rebuilt, not served from the cache; the test no longer models a cached body")
	}
	if idx := responseIndex(t, second); idx >= changeSeq {
		t.Fatalf("cached body omits bead %s but X-GC-Index = %d, want < %d", b.ID, idx, changeSeq)
	}
}

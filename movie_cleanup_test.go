package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRadarrMovies serves a small Radarr: tags, the movie list, and movie deletion.
type fakeRadarrMovies struct {
	mu      sync.Mutex
	movies  []map[string]any
	deleted []string // movie ids
}

func movieApp(t *testing.T) (*App, *fakeRadarrMovies, string) {
	t.Helper()
	dl := t.TempDir()
	file := func(p string) string {
		full := filepath.Join(dl, p)
		os.MkdirAll(filepath.Dir(full), 0755)
		os.WriteFile(full, []byte("movie"), 0644)
		return full
	}
	f := &fakeRadarrMovies{movies: []map[string]any{
		{"id": 1, "tmdbId": 1377237, "title": "Runner", "year": 2026, "tags": []int{1}, "hasFile": true,
			"movieFile": map[string]any{"path": file("ravi/movies/Runner (2026)/Runner (2026) (WEBDL-1080p).mkv"), "size": 5}},
		{"id": 2, "tmdbId": 222, "title": "Keeper", "year": 2025, "tags": []int{1, 9}, "hasFile": true,
			"movieFile": map[string]any{"path": file("ravi/movies/Keeper (2025)/Keeper (2025).mkv"), "size": 5}},
		{"id": 3, "tmdbId": 333, "title": "Her Film", "year": 2024, "tags": []int{2}, "hasFile": true,
			"movieFile": map[string]any{"path": file("daniela/movies/Her Film (2024)/Her Film (2024).mkv"), "size": 5}},
		{"id": 4, "tmdbId": 444, "title": "Masterplan", "year": 2026, "tags": []int{1}, "hasFile": true,
			"movieFile": map[string]any{"path": file("ravi/movies/Masterplan (2026)/Masterplan (2026).mkv"), "size": 5}},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v3/tag":
			json.NewEncoder(w).Encode([]SonarrTag{{ID: 1, Label: "1-mallox"}, {ID: 2, Label: "2-daniela"}, {ID: 9, Label: "archive"}})
		case r.URL.Path == "/api/v3/movie" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(f.movies)
		case strings.HasPrefix(r.URL.Path, "/api/v3/movie/") && r.Method == http.MethodDelete:
			if r.URL.Query().Get("deleteFiles") != "true" || r.URL.Query().Get("addImportExclusion") != "true" {
				t.Errorf("delete query %s", r.URL.RawQuery)
			}
			f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/api/v3/movie/"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a := newTestApp(t)
	a.Radarr = NewArr(srv.URL, "k")
	a.Config.UserFolders = map[string]string{"1-mallox": filepath.Join(dl, "ravi"), "2-daniela": filepath.Join(dl, "daniela")}
	a.Config.Watchers = houseWatchers
	a.Config.MovieDeleteUsers = []string{"1-mallox"}
	a.Config.MovieArchiveTag = "archive"
	a.Config.MovieSubdir = "movies"
	a.Config.DeleteRoots = []string{dl}
	return a, f, dl
}

func watchMovie(a *App, user, title string, year, tmdb int, ago time.Duration) {
	a.watchedAt(WatchEvent{Source: "jellyfin", Type: "movie", Title: title, Year: year, TmdbID: tmdb, User: user}, a.now().Add(-ago))
}

func (f *fakeRadarrMovies) deletedIDs() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.deleted, ",")
}

func TestWatchedMovieDeletedAfterGrace(t *testing.T) {
	a, f, _ := movieApp(t)
	watchMovie(a, "mallox", "Runner", 2026, 1377237, 2*time.Hour)
	a.ProcessDue()
	if f.deletedIDs() != "" || len(a.Queue.Items()) != 1 {
		t.Fatalf("within the grace period: deleted %q, queue %+v", f.deletedIDs(), a.Queue.Items())
	}
	if !a.Queue.MovieIDs()[1377237] {
		t.Error("a queued movie counts as watched for the upgrade search")
	}
	later := a.now().Add(23 * time.Hour)
	a.Now = func() time.Time { return later }
	a.ProcessDue()
	if f.deletedIDs() != "1" || len(a.Queue.Items()) != 0 {
		t.Fatalf("after the grace period: deleted %q, queue %+v", f.deletedIDs(), a.Queue.Items())
	}
}

func TestWatchedMovieKept(t *testing.T) {
	cases := []struct {
		name, user, title string
		year, tmdb        int
	}{
		{"tagged archive", "mallox", "Keeper", 2025, 222},
		{"Daniela's folder, deletion only for Ravi", "Daniela", "Her Film", 2024, 333},
		{"Ravi's movie, watched only by Daniela", "Daniela", "Runner", 2026, 1377237},
		{"unknown viewer", "", "Runner", 2026, 1377237},
		{"not in Radarr (archive)", "mallox", "Heat", 1995, 949},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, f, _ := movieApp(t)
			watchMovie(a, c.user, c.title, c.year, c.tmdb, 30*time.Hour)
			a.ProcessDue()
			if f.deletedIDs() != "" {
				t.Fatalf("deleted %q", f.deletedIDs())
			}
			if len(a.Queue.Items()) != 0 {
				t.Fatalf("a kept movie leaves the queue: %+v", a.Queue.Items())
			}
		})
	}
}

func TestMovieMatchedByTitleWithoutTmdb(t *testing.T) {
	a, f, _ := movieApp(t)
	watchMovie(a, "mallox", "Masterplan", 2026, 0, 25*time.Hour)
	a.ProcessDue()
	if f.deletedIDs() != "4" {
		t.Fatalf("deleted %q, want Masterplan (4)", f.deletedIDs())
	}
}

func TestMovieDeletionOff(t *testing.T) {
	a, f, _ := movieApp(t)
	a.Config.MovieDeleteUsers = nil
	watchMovie(a, "mallox", "Runner", 2026, 1377237, 30*time.Hour)
	a.ProcessDue()
	if f.deletedIDs() != "" || len(a.Queue.Items()) != 0 {
		t.Fatalf("without MOVIE_DELETE_USERS nothing is queued or deleted: deleted %q, queue %+v", f.deletedIDs(), a.Queue.Items())
	}
}

func TestMovieDeletionDryRun(t *testing.T) {
	a, f, _ := movieApp(t)
	a.Config.DryRun = true
	watchMovie(a, "mallox", "Runner", 2026, 1377237, 30*time.Hour)
	a.ProcessDue()
	if f.deletedIDs() != "" {
		t.Fatalf("dry run deleted %q", f.deletedIDs())
	}
}

func TestMovieWebhooksCarryTmdb(t *testing.T) {
	a, _, _ := movieApp(t)
	rec := httptest.NewRecorder()
	a.handleJellyfin(rec, jellyfinRequest(`{"NotificationType":"PlaybackStop","ItemType":"Movie","Name":"Runner","Year":2026,`+
		`"Provider_tmdb":"1377237","PlayedToCompletion":true,"NotificationUsername":"mallox"}`))
	rec = httptest.NewRecorder()
	a.handlePlex(rec, plexRequest(t, `{"event":"media.scrobble","Account":{"title":"mallocator"},"Metadata":{"type":"movie",`+
		`"title":"Masterplan","year":2026,"guid":"plex://movie/5d776","Guid":[{"id":"imdb://tt1"},{"id":"tmdb://444"}]}}`))
	var got []string
	for _, it := range a.Queue.Items() {
		got = append(got, fmt.Sprintf("%s tmdb %d by %s", it, it.TmdbID, it.Watchers[0].User))
	}
	sort.Strings(got) // queued at the same moment: no fixed order
	want := "Masterplan (2026) tmdb 444 by mallocator|Runner (2026) tmdb 1377237 by mallox"
	if strings.Join(got, "|") != want {
		t.Fatalf("queue %q, want %q", strings.Join(got, "|"), want)
	}
}

func TestQueueFileKeepsMovies(t *testing.T) {
	a, _, _ := movieApp(t)
	watchMovie(a, "mallox", "Runner", 2026, 1377237, time.Hour)
	q, err := LoadQueue(a.Queue.path)
	if err != nil {
		t.Fatal(err)
	}
	items := q.Items()
	if len(items) != 1 || items[0].Kind != "movie" || items[0].TmdbID != 1377237 || items[0].String() != "Runner (2026)" {
		t.Fatalf("reloaded %+v", items)
	}
}

func TestWatchedPollQueuesMovies(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Users":
			json.NewEncoder(w).Encode([]map[string]string{{"Id": "u1", "Name": "mallox"}})
		case strings.HasSuffix(r.URL.Path, "/Items") && r.URL.Query().Get("IncludeItemTypes") == "Movie":
			json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{
				{"Id": "m1", "Name": "Runner", "ProductionYear": 2026, "ProviderIds": map[string]string{"Tmdb": "1377237"},
					"UserData": map[string]any{"LastPlayedDate": now.Add(-time.Hour).Format(time.RFC3339Nano)}},
			}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"Items": []any{}})
		}
	}))
	defer jf.Close()
	dir := t.TempDir()
	q, _ := LoadQueue(filepath.Join(dir, "pending.json"))
	os.WriteFile(filepath.Join(dir, "seen.json"), []byte("{}"), 0644) // not the first run
	a := &App{Config: Config{WatchedLookback: 48 * time.Hour, WatchedMaxAge: 4 * time.Hour, WatchedBurstMax: 3,
		WatchedSeenFile: filepath.Join(dir, "seen.json"), GracePeriod: 24 * time.Hour, MovieDeleteUsers: []string{"1-mallox"}},
		Queue: q, Jellyfin: NewJellyfin(jf.URL, "k"), Now: func() time.Time { return now }}
	a.WatchedPoll()
	items := a.Queue.Items()
	if len(items) != 1 || items[0].Kind != "movie" || items[0].TmdbID != 1377237 || items[0].Source != "jellyfin-poll" {
		t.Fatalf("queue %+v", items)
	}
}

// Plex items carry "guid" (a string) next to "Guid"; v2.18.0 failed on that, so every poll with a watched movie
// (and every Plex webhook) was rejected.
func TestPlexWatchedMoviesWithGuidString(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Directory": []map[string]string{{"key": "1", "type": "movie"}}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Metadata": []map[string]any{
			{"ratingKey": "7", "type": "movie", "title": "Genius", "year": 2016, "lastViewedAt": now.Add(-time.Hour).Unix(),
				"guid": "plex://movie/5d776", "Guid": []map[string]string{{"id": "imdb://tt1"}, {"id": "tmdb://339397"}}},
		}}})
	}))
	defer px.Close()
	got, err := NewPlex(px.URL, "/volume1/Video").WatchedMovies(now.Add(-48 * time.Hour))
	if err != nil || len(got) != 1 || got[0].TmdbID != 339397 || got[0].Title != "Genius" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

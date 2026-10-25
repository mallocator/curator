package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func upRel(title string, res int, gb float64, approved bool) arrRelease {
	r := arrRelease{GUID: title, Title: title, Size: int64(gb * (1 << 30)), Approved: approved}
	r.Quality.Quality.Resolution = res
	return r
}

func TestBetterThan(t *testing.T) {
	have := []mediaVersion{{1080, 8 << 30}, {720, 2 << 30}}
	cases := []struct {
		r    arrRelease
		want bool
	}{
		{upRel("2160p", 2160, 15, true), true},
		{upRel("1080p bigger", 1080, 12.5, true), true}, // >= 1.5 x 8 GB
		{upRel("1080p slightly bigger", 1080, 9, true), false},
		{upRel("720p", 720, 6, true), false},
	}
	for _, c := range cases {
		if got := betterThan(c.r, have); got != c.want {
			t.Errorf("%s: better = %v, want %v", c.r.Title, got, c.want)
		}
	}
	if !betterThan(upRel("anything", 480, 1, true), nil) {
		t.Error("with no known version any release is better")
	}
	r, ok := pickUpgrade([]arrRelease{upRel("2160p not approved", 2160, 20, false), upRel("1080p", 1080, 9, true), upRel("2160p", 2160, 18, true)}, have)
	if !ok || r.Title != "2160p" {
		t.Errorf("picked %q, want the first approved release that is better", r.Title)
	}
	if resolutionClass(800, 1920) != 1080 || resolutionClass(1600, 3840) != 2160 || resolutionClass(536, 1280) != 720 {
		t.Error("resolution classes from frame size")
	}
}

// fakeUpgradeEnv serves Seerr, Radarr and Jellyfin for the upgrade flow and records what curator did.
type fakeUpgradeEnv struct {
	mu          sync.Mutex
	issues      []map[string]any
	comments    []string
	resolved    []int
	movies      map[int]map[string]any // radarr id -> movie
	exclusions  []map[string]any
	releases    []arrRelease
	grabbed     []string
	deleted     []string
	tagsCreated []string
	versions    []map[string]any // jellyfin media sources
}

func newFakeUpgradeEnv(t *testing.T) (*fakeUpgradeEnv, *httptest.Server) {
	env := &fakeUpgradeEnv{movies: map[int]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.mu.Lock()
		defer env.mu.Unlock()
		p := r.URL.Path
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case p == "/api/v1/issue":
			write(map[string]any{"pageInfo": map[string]any{"results": len(env.issues)}, "results": env.issues})
		case strings.HasSuffix(p, "/comment"):
			env.comments = append(env.comments, body["message"].(string))
		case strings.HasSuffix(p, "/resolved"):
			env.resolved = append(env.resolved, 1)
		case p == "/api/v3/movie" && r.Method == "GET":
			var out []map[string]any
			for _, m := range env.movies {
				if r.URL.Query().Get("tmdbId") == "" || jsonInt(m["tmdbId"]) == atoi(r.URL.Query().Get("tmdbId")) {
					out = append(out, m)
				}
			}
			write(out)
		case p == "/api/v3/movie" && r.Method == "POST":
			body["id"] = 99
			env.movies[99] = body
			write(body)
		case strings.HasPrefix(p, "/api/v3/movie/lookup/tmdb"):
			write(map[string]any{"title": "Face/Off", "year": 1997, "tmdbId": 754})
		case strings.HasPrefix(p, "/api/v3/movie/") && r.Method == "GET":
			id := atoi(strings.TrimPrefix(p, "/api/v3/movie/"))
			if m, ok := env.movies[id]; ok {
				write(m)
			} else {
				http.Error(w, "not found", 404)
			}
		case strings.HasPrefix(p, "/api/v3/movie/") && r.Method == "DELETE":
			env.deleted = append(env.deleted, p+"?"+r.URL.RawQuery)
			delete(env.movies, atoi(strings.TrimPrefix(p, "/api/v3/movie/")))
		case p == "/api/v3/exclusions":
			write(env.exclusions)
		case strings.HasPrefix(p, "/api/v3/exclusions/") && r.Method == "DELETE":
			env.exclusions = nil
		case p == "/api/v3/qualityprofile":
			write([]map[string]any{{"id": 4, "name": "HD-1080p"}, {"id": 7, "name": "4K preferred"}})
		case p == "/api/v3/rootfolder":
			write([]map[string]any{{"path": "/downloads/ravi/movies"}})
		case p == "/api/v3/tag" && r.Method == "GET":
			write([]map[string]any{{"id": 1, "label": "1-mallox"}, {"id": 2, "label": "2-daniela"}})
		case p == "/api/v3/tag" && r.Method == "POST":
			env.tagsCreated = append(env.tagsCreated, body["label"].(string))
			write(map[string]any{"id": 3, "label": body["label"]})
		case p == "/api/v3/release" && r.Method == "GET":
			write(env.releases)
		case p == "/api/v3/release" && r.Method == "POST":
			env.grabbed = append(env.grabbed, body["guid"].(string))
		case p == "/Items":
			write(map[string]any{"Items": []map[string]any{{"Path": "/volume1/Video/Movies/Face Off (1997).mkv",
				"ProviderIds": map[string]string{"Tmdb": "754"}, "MediaSources": env.versions}}})
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			http.Error(w, "unexpected", 500)
		}
	}))
	t.Cleanup(srv.Close)
	return env, srv
}

func jsonInt(v any) int {
	f, _ := v.(float64)
	if i, ok := v.(int); ok {
		return i
	}
	return int(f)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func newUpgradeApp(t *testing.T, url string) (*App, *time.Time) {
	a := newTestApp(t)
	now := time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	a.Seerr = NewSeerr(url, "k", 1)
	a.Radarr = NewArr(url, "k")
	a.Jellyfin = NewJellyfin(url, "k")
	a.Config.SeerrUpgradeProfile, a.Config.SeerrUpgradeDays = "4K preferred", 14
	a.Config.SeerrUpgradeStateFile = filepath.Join(t.TempDir(), "seerr-upgrades.json")
	a.Config.MovieArchiveTag, a.Config.MovieArchiveHostPath = "archive", "/volume1/Video/Movies"
	a.Config.UserFolders = map[string]string{"1-mallox": "/downloads/ravi", "2-daniela": "/downloads/daniela"}
	return a, &now
}

func videoIssue(id, tmdb, user int) map[string]any {
	return map[string]any{"id": id, "issueType": 1, "status": 1, "media": map[string]any{"tmdbId": tmdb, "mediaType": "movie"},
		"createdBy": map[string]any{"id": user, "displayName": "x"}}
}

func TestSeerrUpgradeArchivedMovie(t *testing.T) {
	env, srv := newFakeUpgradeEnv(t)
	a, _ := newUpgradeApp(t, srv.URL)
	env.issues = []map[string]any{videoIssue(5, 754, 2),
		{"id": 6, "issueType": 2, "status": 1, "media": map[string]any{"tmdbId": 754, "mediaType": "movie"}}, // audio: ignored
		{"id": 7, "issueType": 1, "status": 1, "media": map[string]any{"tmdbId": 1399, "mediaType": "tv"}}}   // show: ignored
	env.exclusions = []map[string]any{{"id": 16, "tmdbId": 754}}
	env.versions = []map[string]any{{"Size": 8 << 30, "MediaStreams": []map[string]any{{"Type": "Video", "Height": 800, "Width": 1920}}}}
	env.releases = []arrRelease{upRel("FaceOff.2160p.notapproved", 2160, 30, false), upRel("FaceOff.1080p", 1080, 9, true), upRel("FaceOff.2160p", 2160, 25, true)}

	a.SeerrUpgrades()
	if len(env.exclusions) != 0 || env.movies[99] == nil {
		t.Fatalf("exclusion lifted and movie added: exclusions %v movies %v", env.exclusions, env.movies)
	}
	m := env.movies[99]
	if m["monitored"] != false || jsonInt(m["qualityProfileId"]) != 7 || m["rootFolderPath"] != "/downloads/ravi/movies" {
		t.Errorf("added movie settings: %v", m)
	}
	if tags := m["tags"].([]any); len(tags) != 2 || jsonInt(tags[0]) != 3 || jsonInt(tags[1]) != 2 {
		t.Errorf("tags %v, want the archive tag (created) and the reporter's 2-daniela", tags)
	}
	if len(env.tagsCreated) != 1 || env.tagsCreated[0] != "archive" {
		t.Errorf("tags created %v", env.tagsCreated)
	}
	if len(env.grabbed) != 1 || env.grabbed[0] != "FaceOff.2160p" {
		t.Fatalf("grabbed %v, want the approved 2160p release", env.grabbed)
	}
	if len(env.comments) != 1 || !strings.Contains(env.comments[0], "Downloading a better version") || !strings.Contains(env.comments[0], "1080p, 8.0 GB") {
		t.Errorf("comments %v", env.comments)
	}
	// still downloading: nothing happens
	a.SeerrUpgrades()
	if len(env.comments) != 1 || len(env.resolved) != 0 {
		t.Errorf("no news while downloading: %v %v", env.comments, env.resolved)
	}
	// the archive step removed it from Radarr: done
	delete(env.movies, 99)
	a.SeerrUpgrades()
	if len(env.resolved) != 1 || !strings.Contains(env.comments[len(env.comments)-1], "Done") {
		t.Errorf("resolved %v comments %v", env.resolved, env.comments)
	}
}

func TestSeerrUpgradeGivesUpAndRestores(t *testing.T) {
	env, srv := newFakeUpgradeEnv(t)
	a, now := newUpgradeApp(t, srv.URL)
	env.issues = []map[string]any{videoIssue(8, 754, 1)}
	env.versions = []map[string]any{{"Size": 20 << 30, "MediaStreams": []map[string]any{{"Type": "Video", "Height": 2160, "Width": 3840}}}}
	env.releases = []arrRelease{upRel("FaceOff.2160p.small", 2160, 15, true)}

	a.SeerrUpgrades()
	a.SeerrUpgrades() // same day: no second search, no second comment
	if len(env.grabbed) != 0 || len(env.comments) != 1 || !strings.Contains(env.comments[0], "nothing better is available") {
		t.Fatalf("grabbed %v comments %v", env.grabbed, env.comments)
	}
	*now = now.Add(15 * 24 * time.Hour)
	a.SeerrUpgrades()
	if len(env.deleted) != 1 || !strings.Contains(env.deleted[0], "addImportExclusion=true") || env.movies[99] != nil {
		t.Errorf("movie curator added is removed again with its exclusion: %v", env.deleted)
	}
	if !strings.Contains(env.comments[len(env.comments)-1], "stopped looking") {
		t.Errorf("comments %v", env.comments)
	}
}

func TestSeerrUpgradeIssueClosedByHand(t *testing.T) {
	env, srv := newFakeUpgradeEnv(t)
	a, _ := newUpgradeApp(t, srv.URL)
	env.issues = []map[string]any{videoIssue(9, 754, 1)}
	env.releases = nil
	a.SeerrUpgrades()
	env.issues = nil // closed in Seerr
	a.SeerrUpgrades()
	if len(env.deleted) != 1 {
		t.Errorf("movie added for a closed issue is removed again: %v", env.deleted)
	}
	if st := a.seerrUpgradeLog().items[9].Stage; st != "closed" {
		t.Errorf("stage %q", st)
	}
}

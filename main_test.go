package main

import (
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"net/http"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gorilla/feeds"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("failed opening test db: %v", err)
	}
	err = db.AutoMigrate(&Event{})
	if err != nil {
		t.Fatalf("failed migrating test db: %v", err)
	}
	return db
}

func TestCheckDuplicate_InSlice(t *testing.T) {
	db := openTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	}()

	events := []Event{{Hash: "h1"}}
	ev := &Event{Hash: "h1"}

	got, err := checkDuplicate(ev, db, &events)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Fatalf("expected duplicate in slice, got false")
	}
}

func TestCheckDuplicate_InDB(t *testing.T) {
	db := openTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	}()

	db.Create(&Event{Hash: "h2", Title: "t"})

	events := []Event{}
	ev := &Event{Hash: "h2"}

	got, err := checkDuplicate(ev, db, &events)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Fatalf("expected duplicate in db, got false")
	}
}

func TestCheckDuplicate_NotDuplicate(t *testing.T) {
	db := openTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	}()

	events := []Event{}
	ev := &Event{Hash: "h3"}

	got, err := checkDuplicate(ev, db, &events)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatalf("expected not duplicate, got true")
	}
}

func TestPruneEvents(t *testing.T) {
	db := openTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	}()

	old := Event{
		Title:    "old",
		DateTime: time.Now().AddDate(-6, 0, 0).Unix(),
		Hash:     "oldhash",
	}
	newE := Event{
		Title:    "new",
		DateTime: time.Now().Unix(),
		Hash:     "newhash",
	}

	if err := db.Create(&old).Error; err != nil {
		t.Fatalf("create old event failed: %v", err)
	}
	if err := db.Create(&newE).Error; err != nil {
		t.Fatalf("create new event failed: %v", err)
	}

	if err := pruneEvents(db); err != nil {
		t.Fatalf("pruneEvents returned error: %v", err)
	}

	var remaining []Event
	if err := db.Find(&remaining).Error; err != nil {
		t.Fatalf("find remaining failed: %v", err)
	}

	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining event, got %d", len(remaining))
	}
	if remaining[0].Hash != "newhash" {
		t.Fatalf("expected newhash remaining, got %s", remaining[0].Hash)
	}
}

func TestTranslateEventToItem(t *testing.T) {
	e := &Event{
		Title:       "MyTitle",
		Description: "Desc",
		Location:    "Mitte",
		Link:        "https://example.com/1",
		DateTime:    time.Date(2020, 5, 1, 12, 0, 0, 0, time.UTC).Unix(),
		Hash:        "thehash",
	}

	item, err := translateEventToItem(e)
	if err != nil {
		t.Fatalf("translateEventToItem error: %v", err)
	}
	if item.Id != e.Hash {
		t.Fatalf("expected id %s, got %s", e.Hash, item.Id)
	}
	if item.Title != e.Title {
		t.Fatalf("expected title %s, got %s", e.Title, item.Title)
	}
	if item.Link == nil || item.Link.Href != e.Link {
		t.Fatalf("expected link %s, got %v", e.Link, item.Link)
	}
	if !strings.Contains(item.Description, e.Description) {
		t.Fatalf("description missing original: %s", item.Description)
	}
	if !strings.Contains(item.Description, "Bezirk: "+e.Location) {
		t.Fatalf("description missing location: %s", item.Description)
	}
	if !item.Created.Equal(time.Unix(e.DateTime, 0)) {
		t.Fatalf("created mismatch, expected %v got %v", time.Unix(e.DateTime, 0), item.Created)
	}
}

func withServerClient(t *testing.T, server *httptest.Server, fn func()) {
	t.Helper()
	orig := globalClient.client
	globalClient.client = server.Client()
	defer func() { globalClient.client = orig }()
	fn()
}

func TestExtractMetaTags_Success(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprintln(w, `<!doctype html><html><head>
            <meta name="description" content="desc">
            <meta property="og:title" content="otitle">
            </head><body>ok</body></html>`)
	}
	server := httptest.NewServer(http.HandlerFunc(handler))
	defer server.Close()

	withServerClient(t, server, func() {
		t.Log("calling extractMetaTags on", server.URL)
		tags, err := extractMetaTags(server.URL)
		if err != nil {
			t.Fatalf("extractMetaTags error: %v", err)
		}
		if len(tags) < 2 {
			t.Fatalf("expected at least 2 meta tags, got %d", len(tags))
		}
		foundDesc := false
		foundOG := false
		for _, mt := range tags {
			if mt.Name == "description" && mt.Content == "desc" {
				foundDesc = true
			}
			if mt.Name == "og:title" && mt.Content == "otitle" {
				foundOG = true
			}
		}
		if !foundDesc {
			t.Fatalf("description meta not found or incorrect")
		}
		if !foundOG {
			t.Fatalf("og:title meta not found or incorrect")
		}
	})
}

func TestExtractMetaTags_RetryThenSuccess(t *testing.T) {
	var calls int
	handler := func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(500)
			fmt.Fprintln(w, "error")
			return
		}
		w.WriteHeader(200)
		fmt.Fprintln(w, `<!doctype html><html><head>
            <meta name="description" content="afterretry">
            </head><body>ok</body></html>`)
	}
	server := httptest.NewServer(http.HandlerFunc(handler))
	defer server.Close()

	withServerClient(t, server, func() {
		tags, err := extractMetaTags(server.URL)
		if err != nil {
			t.Fatalf("extractMetaTags expected success after retry, got error: %v", err)
		}
		if len(tags) == 0 {
			t.Fatalf("expected tags after retry, got none")
		}
		found := false
		for _, mt := range tags {
			if mt.Name == "description" && mt.Content == "afterretry" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected description 'afterretry', not found")
		}
	})
}

func TestFeedsIntegrationSanity(t *testing.T) {
	e := &Event{
		Title:       "X",
		Description: "Y",
		Location:    "L",
		Link:        "https://x",
		DateTime:    time.Now().Unix(),
		Hash:        "h",
	}
	it, err := translateEventToItem(e)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	feed := &feeds.Feed{
		Title:       "t",
		Link:        &feeds.Link{Href: "u"},
		Description: "d",
		Author:      &feeds.Author{Name: "A"},
		Created:     time.Now(),
	}
	feed.Add(it)
	_, err = feed.ToRss()
	if err != nil {
		t.Fatalf("ToRss failed: %v", err)
	}
	_, err = feed.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON failed: %v", err)
	}
	_, err = feed.ToAtom()
	if err != nil {
		t.Fatalf("ToAtom failed: %v", err)
	}
}

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	orig := os.Getenv("WEB_PORT")
	_ = os.Unsetenv("WEB_PORT")
	code := m.Run()
	if orig != "" {
		_ = os.Setenv("WEB_PORT", orig)
	}
	os.Exit(code)
}

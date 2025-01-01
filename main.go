package main

import (
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/feeds"

	"github.com/PuerkitoBio/goquery"

	"github.com/gocolly/colly/v2"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Event struct {
	gorm.Model
	Title       string
	Description string
	Location    string
	Link        string
	DateTime    int64
	Hash        string `gorm:"unique"`
}

type MetaTag struct {
	Name    string
	Content string
}

func checkDuplicate(event *Event, db *gorm.DB, events *[]Event) (bool, error) {
	var existingEvent Event

	eventIdx := slices.IndexFunc(*events, func(e Event) bool { return e.Hash == event.Hash })

	if eventIdx != -1 {
		return true, nil
	}

	err := db.First(&existingEvent, &Event{Hash: event.Hash}).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return true, nil
}

func pruneEvents(db *gorm.DB) error {
	lastTime := time.Now().AddDate(0, -6, 0).Unix()
	result := db.Where("date_time < ?", lastTime).Delete(&Event{})
	if result.Error != nil {
		return result.Error
	}
	return nil
}

func translateEventToItem(event *Event) (*feeds.Item, error) {
	feederItem := feeds.Item{
		Title:       event.Title,
		Link:        &feeds.Link{Href: event.Link},
		Description: event.Description + "\n\nBezirk: " + event.Location,
		Author:      &feeds.Author{Name: "Presseabteilung", Email: "pressestelle@polizei.berlin.de"},
		Created:     time.Unix(event.DateTime, 0),
	}
	return &feederItem, nil
}

func extractMetaTags(url string) ([]MetaTag, error) {
	res, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, errors.New(res.Status)
	}
	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		return nil, err
	}
	var metaTags []MetaTag
	doc.Find("meta").Each(func(i int, s *goquery.Selection) {
		metaTag := MetaTag{}
		if name, exists := s.Attr("name"); exists {
			metaTag.Name = name
			metaTag.Content = s.AttrOr("content", "")
		} else if property, exists := s.Attr("property"); exists {
			metaTag.Name = property
			metaTag.Content = s.AttrOr("content", "")
		}
		metaTags = append(metaTags, metaTag)
	})
	return metaTags, nil
}

func main() {
	log.Println("Initializing police scraper...")

	db, err := gorm.Open(sqlite.Open("/data/policeEvents.db"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		log.Fatal(err)
	}

	err = db.AutoMigrate(&Event{})
	if err != nil {
		log.Fatal(err)
	}

	err = pruneEvents(db)
	if err != nil {
		log.Fatal(err)
	}

	feed := &feeds.Feed{
		Title:       "Berliner Polizeimeldungen",
		Link:        &feeds.Link{Href: "https://www.berlin.de/polizei/polizeimeldungen/"},
		Description: "Ein RSS Feed für Berliner Polizeimeldungen",
		Author:      &feeds.Author{Name: "Aron", Email: "github@luiggi33.de"},
		Created:     time.Now(),
	}

	var oldEvents []Event
	db.Find(&oldEvents)

	for _, event := range oldEvents {
		translatedEvent, _ := translateEventToItem(&event)
		feed.Add(translatedEvent)
	}

	feedRSS, _ := feed.ToRss()
	feedJSON, _ := feed.ToJSON()
	feedAtom, _ := feed.ToAtom()

	mainCollector := colly.NewCollector(
		colly.AllowedDomains("www.berlin.de"),
	)

	mainCollector.OnRequest(func(r *colly.Request) {
		log.Println("Visiting:", r.URL)
	})

	mainCollector.OnError(func(_ *colly.Response, err error) {
		log.Println("Something went wrong:", err)
	})

	var events []Event

	mainCollector.OnResponse(func(r *colly.Response) {
		events = nil
	})

	mainCollector.OnHTML("ul.list--tablelist > li", func(e *colly.HTMLElement) {
		event := Event{}

		t, err := time.Parse("02.01.2006 15:04 Uhr", e.ChildText("div.cell.nowrap.date"))
		if err != nil {
			log.Println("Error parsing date:", err)
			return
		}
		event.DateTime = t.Unix()
		event.Title = e.ChildText("a")
		event.Link = "https://www.berlin.de" + e.ChildAttr("a", "href")
		event.Location = strings.TrimPrefix(e.ChildText("span.category"), "Ereignisort: ")

		hash := adler32.Checksum([]byte(event.Title + strconv.FormatInt(event.DateTime, 10)))
		event.Hash = fmt.Sprintf("%x", hash)

		exists, _ := checkDuplicate(&event, db, &events)
		if exists {
			return
		}

		metaTags, _ := extractMetaTags(event.Link)
		descriptionIdx := slices.IndexFunc(metaTags, func(tag MetaTag) bool { return tag.Name == "description" })
		event.Description = metaTags[descriptionIdx].Content

		events = append(events, event)
	})

	mainCollector.OnScraped(func(r *colly.Response) {
		log.Printf("%s scraped, collected %d events!", r.Request.URL, len(events))

		newEvents := 0

		for _, event := range events {
			db.Create(&event)
			translatedEvent, _ := translateEventToItem(&event)
			feed.Add(translatedEvent)
			newEvents++
		}

		if len(events) > 0 {
			feedRSS, _ = feed.ToRss()
			feedJSON, _ = feed.ToJSON()
			feedAtom, _ = feed.ToAtom()
		}

		log.Printf("Added %d new events", newEvents)
	})

	// TODO maybe initially scrape all the pages
	err = mainCollector.Visit("https://www.berlin.de/polizei/polizeimeldungen")
	if err != nil {
		log.Fatal(err)
		return
	}

	ticker := time.NewTicker(1 * time.Hour)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				err = mainCollector.Visit("https://www.berlin.de/polizei/polizeimeldungen")
				if err != nil {
					log.Fatal(err)
					return
				}
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()

	http.HandleFunc("/atom", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		_, err := io.WriteString(w, feedAtom)
		if err != nil {
			log.Println("Error writing atom:", err)
			return
		}
	})
	http.HandleFunc("/rss", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		_, err := io.WriteString(w, feedRSS)
		if err != nil {
			log.Println("Error writing rss:", err)
			return
		}
	})
	http.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, feedJSON)
		if err != nil {
			log.Println("Error writing json:", err)
			return
		}
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/rss", http.StatusSeeOther)
	})

	// TODO make env var for port?
	err = http.ListenAndServe("0.0.0.0:8080", nil)
	if errors.Is(err, http.ErrServerClosed) {
		log.Println("Shutting down...")
	} else if err != nil {
		log.Fatal(err)
	}
}

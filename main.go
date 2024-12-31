package main

import (
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/feeds"
	"gorm.io/gorm/logger"

	"github.com/gocolly/colly/v2"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type Event struct {
	gorm.Model
	Title    string
	Location string
	Link     string
	DateTime int64
	Hash     string `gorm:"unique"`
}

func checkDuplicate(event *Event, db *gorm.DB) (bool, error) {
	var existingEvent Event
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
		Description: "Bezirk: " + event.Location,
		Author:      &feeds.Author{Name: "Presseabteilung", Email: "pressestelle@polizei.berlin.de"},
		Created:     time.Unix(event.DateTime, 0),
	}
	return &feederItem, nil
}

func main() {
	log.Println("Initializing police scraper...")

	db, err := gorm.Open(sqlite.Open("/data/policeEvents.db"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		log.Fatal(err)
	}

	db.AutoMigrate(&Event{})

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

	c := colly.NewCollector(
		colly.AllowedDomains("www.berlin.de"),
	)

	c.OnRequest(func(r *colly.Request) {
		log.Println("Visiting:", r.URL)
	})

	c.OnError(func(_ *colly.Response, err error) {
		log.Println("Something went wrong:", err)
	})

	c.OnResponse(func(r *colly.Response) {
		log.Println("Page visited:", r.Request.URL)
	})

	var events []Event

	c.OnHTML("ul.list--tablelist > li", func(e *colly.HTMLElement) {
		event := Event{}

		t, err := time.Parse("02.01.2006 15:04 Uhr", e.ChildText("div.cell.nowrap.date"))
		if err != nil {
			log.Fatal("Error parsing date:", err)
			return
		}
		event.DateTime = t.Unix()
		event.Title = e.ChildText("a")
		event.Link = "https://www.berlin.de" + e.ChildAttr("a", "href")
		event.Location = strings.TrimPrefix(e.ChildText("span.category"), "Ereignisort: ")

		// TODO add a description that is scraped from the first idk lines of the link

		hash := adler32.Checksum([]byte(event.Title + strconv.FormatInt(event.DateTime, 10)))
		event.Hash = fmt.Sprintf("%x", hash)

		events = append(events, event)
	})

	c.OnScraped(func(r *colly.Response) {
		log.Println(r.Request.URL, "scraped!")

		for _, event := range events {
			exists, _ := checkDuplicate(&event, db)
			if exists {
				continue
			}
			db.Create(&event)
			translatedEvent, _ := translateEventToItem(&event)
			feed.Add(translatedEvent)
		}
	})

	// TODO maybe initially scrape all the pages
	err = c.Visit("https://www.berlin.de/polizei/polizeimeldungen/")
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
				err = c.Visit("https://www.berlin.de/polizei/polizeimeldungen/")
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
		atom, err := feed.ToAtom()
		if err != nil {
			io.WriteString(w, err.Error())
			log.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/atom+xml")
		io.WriteString(w, atom)
	})
	http.HandleFunc("/rss", func(w http.ResponseWriter, r *http.Request) {
		rss, err := feed.ToRss()
		if err != nil {
			io.WriteString(w, err.Error())
			log.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/atom+xml")
		io.WriteString(w, rss)
	})
	http.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		json, err := feed.ToJSON()
		if err != nil {
			io.WriteString(w, err.Error())
			log.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, json)
	})

	// TODO make env var for port?
	err = http.ListenAndServe("0.0.0.0:8080", nil)
	if errors.Is(err, http.ErrServerClosed) {
		log.Println("Shutting down...")
	} else if err != nil {
		log.Fatal(err)
	}
}

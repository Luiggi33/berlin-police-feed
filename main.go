package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"golang.org/x/time/rate"
	"hash/adler32"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
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

type RateLimitedClient struct {
	client      *http.Client
	rateLimiter *rate.Limiter
	mu          sync.Mutex
}

func NewRateLimitedClient(requestsPerSecond float64, burst int) *RateLimitedClient {
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{},
		ForceAttemptHTTP2: false,
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   20 * time.Second, // Increased timeout
	}

	return &RateLimitedClient{
		client:      client,
		rateLimiter: rate.NewLimiter(rate.Limit(requestsPerSecond), burst),
	}
}

func (c *RateLimitedClient) Do(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	err := c.rateLimiter.Wait(req.Context())
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return c.client.Do(req)
}

var globalClient = NewRateLimitedClient(0.5, 1)

func checkDuplicate(event *Event, db *gorm.DB, events *[]Event) (bool, error) {
	eventIdx := slices.IndexFunc(*events, func(e Event) bool { return e.Hash == event.Hash })
	if eventIdx != -1 {
		return true, nil
	}
	var existingEvent Event
	err := db.First(&existingEvent, &Event{Hash: event.Hash}).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return true, nil
}

func pruneEvents(db *gorm.DB) error {
	lastTime := time.Now().AddDate(-5, 0, 0).Unix()
	result := db.Where("date_time < ?", lastTime).Delete(&Event{})
	if result.Error != nil {
		return result.Error
	}
	return nil
}

func translateEventToItem(event *Event) (*feeds.Item, error) {
	feederItem := feeds.Item{
		Id:          event.Hash,
		Title:       event.Title,
		Link:        &feeds.Link{Href: event.Link},
		Description: event.Description + "\n\nBezirk: " + event.Location,
		Author:      &feeds.Author{Name: "Presseabteilung", Email: "pressestelle@polizei.berlin.de"},
		Created:     time.Unix(event.DateTime, 0),
	}
	return &feederItem, nil
}

func extractMetaTags(url string) ([]MetaTag, error) {
	maxRetries := 3
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt)) * time.Second
			jitter := time.Duration(rand.Float64() * float64(backoff))
			time.Sleep(backoff + jitter)
		}

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}

		// Rotate between different user agents to appear more natural
		userAgents := []string{
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
			"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:89.0) Gecko/20100101 Firefox/89.0",
		}
		req.Header.Set("User-Agent", userAgents[attempt%len(userAgents)])
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "en-US,en;q=0.5")
		req.Header.Set("Connection", "keep-alive")

		res, err := globalClient.client.Do(req)
		if err != nil {
			lastErr = err
			log.Printf("Attempt %d failed: %v\n", attempt+1, err)
			continue
		}
		defer res.Body.Close()

		if res.StatusCode != 200 {
			lastErr = errors.New(res.Status)
			log.Printf("Attempt %d failed with status %d\n", attempt+1, res.StatusCode)
			// 429 (Too Many Requests)
			if res.StatusCode == 429 {
				time.Sleep(time.Duration(30+rand.Intn(30)) * time.Second)
			}
			continue
		}

		doc, err := goquery.NewDocumentFromReader(res.Body)
		if err != nil {
			lastErr = err
			continue
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

	return nil, fmt.Errorf("failed after %d attempts, last error: %v", maxRetries, lastErr)
}

func main() {
	log.Println("Initializing police scraper...")

	policeURL, exists := os.LookupEnv("POLICE_URL")

	if !exists {
		policeURL = "https://www.berlin.de/polizei/polizeimeldungen/"
		log.Println("POLICE_URL environment variable not set, defaulting")
	}

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
		Link:        &feeds.Link{Href: policeURL},
		Description: "Ein RSS Feed für Berliner Polizeimeldungen",
		Author:      &feeds.Author{Name: "Aron", Email: "github@luiggi33.de"},
		Created:     time.Now(),
	}

	var events []Event
	db.Find(&events).Limit(250)

	for _, event := range events {
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

	var newEvents []Event

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
		event.Description = "Keine Beschreibung gefunden"

		hash := adler32.Checksum([]byte(event.Title + strconv.FormatInt(event.DateTime, 10)))
		event.Hash = fmt.Sprintf("%x", hash)

		exists, _ := checkDuplicate(&event, db, &events)
		if exists {
			return
		}

		metaTags, err := extractMetaTags(event.Link)
		if err != nil {
			log.Println("Error extracting meta tags:", err)
			return
		}

		descriptionIdx := slices.IndexFunc(metaTags, func(tag MetaTag) bool { return tag.Name == "description" })
		if descriptionIdx != -1 {
			event.Description = metaTags[descriptionIdx].Content
		}

		newEvents = append(newEvents, event)
	})

	mainCollector.OnScraped(func(r *colly.Response) {
		log.Printf("%s scraped, collected %d new events!", r.Request.URL, len(newEvents))

		for _, event := range newEvents {
			err := db.Create(&event).Error
			if err != nil {
				log.Println("Error creating event:", err)
				continue
			}
			translatedEvent, _ := translateEventToItem(&event)
			feed.Add(translatedEvent)
			events = append(events, event)
		}

		if len(newEvents) > 0 {
			feedRSS, _ = feed.ToRss()
			feedJSON, _ = feed.ToJSON()
			feedAtom, _ = feed.ToAtom()

			log.Printf("Added %d new events to feed", len(newEvents))
		}

		newEvents = nil
	})

	// TODO maybe initially scrape all the pages
	err = mainCollector.Visit(policeURL)
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
				err = mainCollector.Visit(policeURL)
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

	webPort, exists := os.LookupEnv("WEB_PORT")

	if !exists {
		webPort = "8080"
		log.Printf("WEB_PORT not set, defaulting to port %s", webPort)
	}

	err = http.ListenAndServe("0.0.0.0:"+webPort, nil)
	if errors.Is(err, http.ErrServerClosed) {
		log.Println("Shutting down...")
	} else if err != nil {
		log.Fatal(err)
	}
}

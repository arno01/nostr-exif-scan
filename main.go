package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	exif "github.com/rwcarlsen/goexif/exif"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

var (
	npubFlag   = flag.String("npub", "", "npub1... public key (required)")
	threads    = flag.Int("threads", 8, "Number of parallel workers (max 32)")
	limit      = flag.Int("limit", 10000, "Maximum number of events to fetch")
	sinceFlag  = flag.String("since", "", "Only fetch events after this RFC3339 timestamp")
	untilFlag  = flag.String("until", "", "Only fetch events before this RFC3339 timestamp")
	verbose    = flag.Bool("v", false, "Verbose output: show full EXIF details")
	maxThreads = 32
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Println("\nExample:")
		fmt.Printf("  %s --npub npub1... --threads 8 --limit 5000 --since 2024-01-01T00:00:00Z --until 2025-01-01T00:00:00Z -v\n", os.Args[0])
	}

	flag.Parse()
	if len(os.Args) == 1 {
		flag.Usage()
		os.Exit(1)
	}

	if *npubFlag == "" {
		fmt.Println("\033[31m❌ Please provide --npub\033[0m")
		os.Exit(1)
	}
	if *threads < 1 || *threads > maxThreads {
		fmt.Printf("\033[31m❌ --threads must be between 1 and %d\033[0m\n", maxThreads)
		os.Exit(1)
	}

	pubkey, err := decodeNpub(*npubFlag)
	if err != nil {
		fmt.Println("\033[31m❌ Invalid npub:\033[0m", err)
		os.Exit(1)
	}

	relays := loadRelays("relays.txt")
	events := fetchEvents(pubkey, relays)
	if len(events) == 0 {
		fmt.Println("ℹ️  No posts found.")
		return
	}

	sort.Slice(events, func(i, j int) bool {
		return events[i].CreatedAt < events[j].CreatedAt
	})

	first := time.Unix(int64(events[0].CreatedAt), 0).Format(time.RFC3339)
	last := time.Unix(int64(events[len(events)-1].CreatedAt), 0).Format(time.RFC3339)

	fmt.Printf("📚 Total posts: \033[36m%d\033[0m\n", len(events))
	fmt.Printf("📅 Oldest post: \033[36m%s\033[0m\n", first)
	fmt.Printf("📅 Newest post: \033[36m%s\033[0m\n", last)

	imagePosts := extractImageLinks(events)
	fmt.Printf("📸 Found \033[36m%d\033[0m image links\n", len(imagePosts))
	scanImages(imagePosts, *threads, *verbose)
}

type imagePost struct {
	ID  string
	URL string
}

func decodeNpub(npub string) (string, error) {
	_, data, err := nip19.Decode(npub)
	if err != nil {
		return "", err
	}
	return data.(string), nil
}

func loadRelays(path string) []string {
	file, err := os.Open(path)
	if err != nil {
		return []string{
			"wss://relay.nostr.band",
			"wss://nos.lol",
			"wss://relay.snort.social",
		}
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var relays []string
	for scanner.Scan() {
		relay := strings.TrimSpace(scanner.Text())
		if relay != "" {
			relays = append(relays, relay)
		}
	}
	return relays
}

func fetchEvents(pubkey string, relays []string) []nostr.Event {
	var events []nostr.Event
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	filter := nostr.Filter{
		Kinds:   []int{1},
		Authors: []string{pubkey},
		Limit:   *limit,
	}

	if *sinceFlag != "" {
		if t, err := time.Parse(time.RFC3339, *sinceFlag); err == nil {
			ts := nostr.Timestamp(t.Unix())
			filter.Since = &ts
		}
	}
	if *untilFlag != "" {
		if t, err := time.Parse(time.RFC3339, *untilFlag); err == nil {
			ts := nostr.Timestamp(t.Unix())
			filter.Until = &ts
		}
	}

	pool := nostr.NewSimplePool(ctx)
	ch := pool.SubManyEose(ctx, relays, nostr.Filters{filter})
	for evt := range ch {
		events = append(events, *evt.Event)
	}
	return events
}

func extractImageLinks(events []nostr.Event) []imagePost {
	var out []imagePost
	imgRE := regexp.MustCompile(`https?://[^\s]+?\.(?i)(jpg|jpeg|png|gif|webp)`)
	for _, evt := range events {
		matches := imgRE.FindAllString(evt.Content, -1)
		for _, url := range matches {
			out = append(out, imagePost{ID: evt.ID, URL: url})
		}
	}
	return out
}

func scanImages(posts []imagePost, threadCount int, verbose bool) {
	sem := make(chan struct{}, threadCount)
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 10 * time.Second}
	total := len(posts)

	sensitiveTags := []exif.FieldName{
		exif.GPSLatitude,
		exif.GPSLongitude,
		exif.GPSAltitude,
		exif.GPSTimeStamp,
		exif.GPSDateStamp,
		exif.GPSImgDirection,
		exif.Model,
		exif.Make,
		exif.DateTimeOriginal,
		exif.FieldName("CreateDate"),
		exif.Software,
		exif.LensModel,
		exif.LensMake,
	}

	for i, post := range posts {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, p imagePost) {
			defer wg.Done()
			fmt.Printf("[%d/%d] 🔎 Checking \033[36m%s\033[0m\n", idx+1, total, p.URL)

			resp, err := client.Get(p.URL)
			if err != nil {
				fmt.Printf("    ❌ Failed to fetch \033[31m%s\033[0m\n", p.URL)
				<-sem
				return
			}
			defer resp.Body.Close()
			buf, err := io.ReadAll(resp.Body)
			if err != nil {
				fmt.Printf("    ❌ Read failed for \033[31m%s\033[0m\n", p.URL)
				<-sem
				return
			}

			r := bytes.NewReader(buf)
			x, err := exif.Decode(r)
			if err != nil {
				<-sem
				return // No EXIF or unreadable
			}

			sensitive := false
			var lat, lon float64
			var latRef, lonRef string

			for _, field := range sensitiveTags {
				if tag, err := x.Get(field); err == nil {
					sensitive = true
					if verbose {
						if field == exif.GPSLatitude || field == exif.GPSLongitude {
							refTag, _ := x.Get(exif.FieldName(string(field) + "Ref"))
							ref, _ := refTag.StringVal()
							num0, denom0, err0 := tag.Rat2(0)
							num1, denom1, err1 := tag.Rat2(1)
							num2, denom2, err2 := tag.Rat2(2)
							if err0 == nil && err1 == nil && err2 == nil {
								deg := float64(num0) / float64(denom0)
								min := float64(num1) / float64(denom1)
								sec := float64(num2) / float64(denom2)
								total := deg + (min / 60) + (sec / 3600)
								if field == exif.GPSLatitude {
									lat = total
									latRef = ref
								} else {
									lon = total
									lonRef = ref
								}
								fmt.Printf("    ➕ %s: %.6f° (%s)\n", field, total, ref)
							}
							continue
						}
						if val, err := tag.StringVal(); err == nil {
							fmt.Printf("    ➕ %s: %s\n", field, val)
						}
					}
				}
			}

			if sensitive {
				nevent, _ := nip19.EncodeEvent(p.ID, nil, "")
				fmt.Printf("🚨 \033[31mSensitive EXIF found\033[0m in post: \033[4mhttps://primal.net/e/%s\033[0m\n", nevent)
				if verbose && lat != 0 && lon != 0 {
					fmt.Printf("    🌍 GPS: https://maps.google.com/?q=%.6f,%+.6f\n", lat*(sign(latRef)), lon*(sign(lonRef)))
				}
			}
			<-sem
		}(i, post)
	}
	wg.Wait()
}

func sign(ref string) float64 {
	switch ref {
	case "S", "W":
		return -1
	default:
		return 1
	}
}


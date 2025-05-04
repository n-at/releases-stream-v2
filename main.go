package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	"gopkg.in/gomail.v2"
)

const PAGE_MAX_LENGTH = 512 * 1024

type Settings struct {
	UserName     string `json:"username"`
	Token        string `json:"token"`
	MailFrom     string `json:"mail_from"`
	MailTo       string `json:"mail_to"`
	MailHost     string `json:"mail_host"`
	MailPort     int    `json:"mail_port"`
	MailSSL      bool   `json:"mail_ssl"`
	MailUsername string `json:"mail_username"`
	MailPassword string `json:"mail_password"`
}

type Repository struct {
	Name            string `json:"name"`
	FullName        string `json:"full_name"`
	Description     string `json:"description"`
	Url             string `json:"html_url"`
	ForksCount      int    `json:"forks_count"`
	StargazersCount int    `json:"stargazers_count"`
	WatchersCount   int    `json:"watchers_count"`
	Owner           struct {
		Login string `json:"login"`
	} `json:"owner"`
}

type Release struct {
	Repository *Repository
	FeedItem   *gofeed.Item
}

type RepositoryReleases struct {
	Repository *Repository
	FeedItems  []*gofeed.Item
}

//go:embed mail.html
var mailTemplate embed.FS

///////////////////////////////////////////////////////////////////////////////

func main() {
	s, err := readSettings()
	if err != nil {
		log.Fatalf("unable to read settings.json: %v", err)
	}

	tpl, err := loadMailTemplate()
	if err != nil {
		log.Fatalf("unable to load mail template: %v", err)
	}

	repositories, err := getStarredRepos(s.UserName, s.Token)
	if err != nil {
		log.Fatalf("unable to get starred repos: %v", err)
	}

	latestIds := readLatestIds()

	var releases []Release

	for _, repository := range repositories {
		log.Printf("reading releases for %s...", repository.FullName)

		releasesFeed, err := getLatestReleases(repository, latestIds[repository.FullName])
		if err != nil {
			log.Printf("unable to read releases for %s: %v", repository.FullName, err)
			continue
		}

		log.Printf("read releases for %s: %d", repository.FullName, len(releasesFeed))

		if len(releasesFeed) > 0 {
			latestIds[repository.FullName] = releasesFeed[0].GUID
		}

		for _, releaseFeedItem := range releasesFeed {
			releases = append(releases, Release{
				Repository: &repository,
				FeedItem:   releaseFeedItem,
			})
		}
	}

	defer writeLatestIds(latestIds)

	pages := splitReleasesByPages(releases)
	log.Printf("got pages to send: %d", len(pages))

	for _, page := range pages {
		pageRepos := extractPageRepositories(page)
		sb := strings.Builder{}
		err := tpl.ExecuteTemplate(&sb, "mail.html", map[string]any{
			"repositories": pageRepos,
		})
		if err != nil {
			log.Printf("unable to render page: %v", err)
			continue
		}

		if err := sendMail(s, sb.String()); err != nil {
			log.Printf("unable to send mail: %v", err)
		}
	}
}

///////////////////////////////////////////////////////////////////////////////

func readSettings() (Settings, error) {
	bytes, err := os.ReadFile("settings.json")
	if err != nil {
		return Settings{}, err
	}

	var s Settings

	if err := json.Unmarshal(bytes, &s); err != nil {
		return Settings{}, err
	}

	return s, nil
}

func readLatestIds() map[string]string {
	ids := make(map[string]string)

	bytes, err := os.ReadFile("latest.json")
	if err != nil {
		log.Printf("unable to read latest.json: %v", err)
		return ids
	}

	if err := json.Unmarshal(bytes, &ids); err != nil {
		log.Printf("unable to read latest.json: %v", err)
		return ids
	}

	return ids
}

func writeLatestIds(ids map[string]string) {
	bytes, err := json.Marshal(ids)
	if err != nil {
		log.Printf("unable to marshal latest.json: %d", err)
		return
	}

	if err := os.WriteFile("latest.json", bytes, 0666); err != nil {
		log.Printf("unable to write latest.json: %v", err)
	}
}

///////////////////////////////////////////////////////////////////////////////

func getStarredRepos(username, token string) ([]Repository, error) {
	var r []Repository

	page := 1

	for {
		url := fmt.Sprintf("https://api.github.com/users/%s/starred?per_page=100&page=%d", username, page)
		log.Printf("reading stars page %d...", page)

		resp, err := sendRequest(url, token)
		if err != nil {
			return nil, fmt.Errorf("unable to send request: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("bad response status: %v", resp.Status)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("unable to read response: %v", err)
		}

		var repos []Repository
		if err := json.Unmarshal(body, &repos); err != nil {
			return nil, fmt.Errorf("unable to parse JSON: %v", err)
		}
		if len(repos) == 0 {
			break
		}

		r = append(r, repos...)

		page++
	}

	slices.SortFunc(r, func(a, b Repository) int {
		return strings.Compare(a.FullName, b.FullName)
	})

	return r, nil
}

func sendRequest(url, token string) (*http.Response, error) {
	client := &http.Client{}
	client.Timeout = 15 * time.Second

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "Go-GitHub-Starred-Script")

	return client.Do(req)
}

///////////////////////////////////////////////////////////////////////////////

func getLatestReleases(r Repository, latestId string) ([]*gofeed.Item, error) {
	fp := gofeed.NewParser()
	feed, err := fp.ParseURL(fmt.Sprintf("%s/releases.atom", r.Url))
	if err != nil {
		return nil, err
	}

	var filtered []*gofeed.Item

	for _, item := range feed.Items {
		if item.GUID == latestId {
			break
		}
		filtered = append(filtered, item)
	}

	return filtered, nil
}

///////////////////////////////////////////////////////////////////////////////

func splitReleasesByPages(releases []Release) [][]Release {
	var pages [][]Release

	var currentPage []Release
	currentPageLength := 0

	for _, release := range releases {
		if len(currentPage) > 0 && currentPageLength+len(release.FeedItem.Content) > PAGE_MAX_LENGTH {
			pages = append(pages, currentPage)
			currentPage = []Release{}
			currentPageLength = 0
		}
		currentPage = append(currentPage, release)
		currentPageLength += len(release.FeedItem.Content)
	}

	if len(currentPage) > 0 {
		pages = append(pages, currentPage)
	}

	return pages
}

func extractPageRepositories(releases []Release) []RepositoryReleases {
	rm := make(map[string]*RepositoryReleases)

	for _, release := range releases {
		rr, ok := rm[release.Repository.FullName]
		if !ok {
			rr = &RepositoryReleases{
				Repository: release.Repository,
			}
			rm[release.Repository.FullName] = rr
		}
		rr.FeedItems = append(rr.FeedItems, release.FeedItem)
	}

	var rr []RepositoryReleases
	for _, r := range rm {
		rr = append(rr, *r)
	}

	slices.SortFunc(rr, func(a, b RepositoryReleases) int {
		return strings.Compare(a.Repository.FullName, b.Repository.FullName)
	})

	return rr
}

///////////////////////////////////////////////////////////////////////////////

func loadMailTemplate() (*template.Template, error) {
	tpl := template.New("mail").Funcs(template.FuncMap{
		"unescape": func(s string) template.HTML {
			return template.HTML(s)
		},
	})

	tpl, err := tpl.ParseFS(mailTemplate, "*.html")
	if err != nil {
		return nil, err
	}

	return tpl, nil
}

func sendMail(s Settings, text string) error {
	msg := gomail.NewMessage()
	msg.SetHeader("From", s.MailFrom)
	msg.SetHeader("To", s.MailTo)
	msg.SetHeader("Subject", "New GitHub Releases")
	msg.SetBody("text/html", text)

	d := gomail.NewDialer(s.MailHost, s.MailPort, s.MailUsername, s.MailPassword)
	if s.MailSSL {
		d.SSL = true
	}

	return d.DialAndSend(msg)
}

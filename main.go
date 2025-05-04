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

const PAGE_MAX_LENGTH = 2 * 1024 * 1024

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
	settings, err := readSettings()
	if err != nil {
		log.Fatalf("unable to read settings.json: %v", err)
	}

	tpl, err := loadMailTemplate()
	if err != nil {
		log.Fatalf("unable to load mail template: %v", err)
	}

	repos, err := getStarredRepos(settings.UserName, settings.Token)
	if err != nil {
		log.Fatalf("unable to get starred repos: %v", err)
	}

	latestIds := readLatestIds()

	var releases []Release

	for _, repo := range repos {
		log.Printf("reading releases for %s...", repo.FullName)

		releasesFeed, err := getLatestReleases(repo, latestIds[repo.FullName])
		if err != nil {
			log.Printf("unable to read releases for %s: %v", repo.FullName, err)
			continue
		}

		log.Printf("read releases for %s: %d", repo.FullName, len(releasesFeed))

		if len(releasesFeed) > 0 {
			latestIds[repo.FullName] = releasesFeed[0].GUID
		}

		for _, releaseFeedItem := range releasesFeed {
			releases = append(releases, Release{
				Repository: &repo,
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

		if err := sendMail(settings, sb.String()); err != nil {
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
	var repos []Repository

	page := 1

	for {
		url := fmt.Sprintf("https://api.github.com/users/%s/starred?per_page=100&page=%d", username, page)
		log.Printf("reading stars page %d...", page)

		res, err := sendRequest(url, token)
		if err != nil {
			return nil, fmt.Errorf("unable to send request: %v", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("bad response status: %v", res.Status)
		}

		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("unable to read response: %v", err)
		}

		var pageRepos []Repository
		if err := json.Unmarshal(body, &pageRepos); err != nil {
			return nil, fmt.Errorf("unable to parse JSON: %v", err)
		}
		if len(pageRepos) == 0 {
			break
		}

		repos = append(repos, pageRepos...)

		page++
	}

	slices.SortFunc(repos, func(a, b Repository) int {
		return strings.Compare(a.FullName, b.FullName)
	})

	return repos, nil
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

func getLatestReleases(repo Repository, latestId string) ([]*gofeed.Item, error) {
	fp := gofeed.NewParser()
	feed, err := fp.ParseURL(fmt.Sprintf("%s/releases.atom", repo.Url))
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
	repos := make(map[string]*RepositoryReleases)

	for _, release := range releases {
		repo, ok := repos[release.Repository.FullName]
		if !ok {
			repo = &RepositoryReleases{
				Repository: release.Repository,
			}
			repos[release.Repository.FullName] = repo
		}
		repo.FeedItems = append(repo.FeedItems, release.FeedItem)
	}

	var rr []RepositoryReleases
	for _, repo := range repos {
		rr = append(rr, *repo)
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

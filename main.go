package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

type Settings struct {
	UserName string `json:"username"`
	Token    string `json:"token"`
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

///////////////////////////////////////////////////////////////////////////////

func main() {
	s, err := readSettings()
	if err != nil {
		log.Fatalf("unable to read settings.json: %v", err)
	}

	r, err := getStarredRepos(s.UserName, s.Token)
	if err != nil {
		log.Fatalf("unable to get starred repos: %v", err)
	}

	for _, item := range r {
		fmt.Printf("%s: %s\n", item.FullName, item.Url)
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

func getStarredRepos(username, token string) ([]Repository, error) {
	var r []Repository

	page := 1

	for {
		url := fmt.Sprintf("https://api.github.com/users/%s/starred?per_page=100&page=%d", username, page)

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

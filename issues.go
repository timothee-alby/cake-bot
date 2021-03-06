package main

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/google/go-github/github"
	log15 "gopkg.in/inconshreveable/log15.v2"
)

const (
	WIPLabel          string = "wip"
	CakedLabel        string = "caked"
	AwaitingCakeLabel string = "awaiting-cake"
)

var (
	IssueUrlRegex  = regexp.MustCompile("repos/([^/]+)/([^/]+)/issues")
	TrelloUrlRegex = regexp.MustCompile("(https?://trello.com/(?:[^\\s()]+))")

	WIPRegex    = regexp.MustCompile("(?i)wip")
	LabelColors = map[string]string{
		// Blue
		WIPLabel: "207de5",
		// Green
		CakedLabel: "009800",
		// Orange
		AwaitingCakeLabel: "eb6420",
	}
	deprecatedLabels = []string{"Awaiting Cake"}
)

func loadComments(client *github.Client, review *ReviewRequest) error {
	var allComments []github.IssueComment

	opts := github.IssueListCommentsOptions{}

	for {
		comments, resp, err := client.Issues.ListComments(*review.repo.Owner.Login, *review.repo.Name, *review.issue.Number, &opts)

		if err != nil {
			return err
		}

		allComments = append(allComments, comments...)

		if resp.NextPage == 0 {
			break
		}

		opts.ListOptions.Page = resp.NextPage
	}

	review.comments = allComments

	return nil
}

func updateIssueReviewLabels(client *github.Client, log log15.Logger, review ReviewRequest) error {
	oldLabels := []string{}
	newLabels := []string{review.CalculateAppropriateStatus()}

	foundReviewLabel, incorrectReviewLabel := false, false

	for _, l := range review.issue.Labels {
		oldLabels = append(oldLabels, *l.Name)

		switch *l.Name {
		case WIPLabel, CakedLabel, AwaitingCakeLabel:
			foundReviewLabel = true

			if *l.Name != newLabels[0] {
				incorrectReviewLabel = true
			}

			continue
		default:
			newLabels = append(newLabels, *l.Name)
		}
	}

	var labelsNeedUpdating bool

	switch {
	case !foundReviewLabel:
		labelsNeedUpdating = true
		log.Info("could not find review label", "old_labels", oldLabels, "new_labels", newLabels)
	case incorrectReviewLabel:
		labelsNeedUpdating = true
		log.Info("review label is incorrect", "old_labels", oldLabels, "new_labels", newLabels)
	default:
		log.Info("review label does not need updating", "labels", oldLabels)
	}

	if labelsNeedUpdating {
		_, _, err := client.Issues.ReplaceLabelsForIssue(*review.repo.Owner.Login, *review.repo.Name, review.Number(), newLabels)

		if err != nil {
			log.Error("unable to update issue review label", "err", err)
			return err
		}
	}

	return nil
}

type Issue struct {
	github.Issue

	Repository github.Repository `json:"repository,omitempty"`
}

type ReviewRequest struct {
	issue    github.Issue
	repo     github.Repository
	comments []github.IssueComment
}

func (p *ReviewRequest) IsWIP() bool {
	return WIPRegex.MatchString(*p.issue.Title)
}

func (p *ReviewRequest) IsCaked() bool {
	for _, c := range p.comments {
		if strings.Contains(*c.Body, ":cake:") {
			return true
		}
	}

	return false
}

func (p *ReviewRequest) CalculateAppropriateStatus() string {
	switch {
	case p.IsWIP():
		return WIPLabel
	case p.IsCaked():
		return CakedLabel
	default:
		return AwaitingCakeLabel
	}
}

func (p *ReviewRequest) ExtractTrelloCardUrls() []string {
	urls := TrelloUrlRegex.FindAllString(*p.issue.Body, -1)

	for _, c := range p.comments {
		urls = append(urls, TrelloUrlRegex.FindAllString(*c.Body, -1)...)
	}

	return urls
}

func (p *ReviewRequest) RepositoryPath() string {
	return fmt.Sprintf("%s/%s", *p.repo.Owner.Login, *p.repo.Name)
}

func (p *ReviewRequest) Number() int {
	return *p.issue.Number
}

func (p *ReviewRequest) URL() string {
	return *p.issue.HTMLURL
}

func ReviewRequestFromIssue(r github.Repository, i github.Issue, c *github.Client) ReviewRequest {
	review := ReviewRequest{
		issue: i,
		repo:  r,
	}

	loadComments(c, &review)

	return review
}

func ghGet(url string, v interface{}) (*github.Response, error) {
	r, err := gh.NewRequest("GET", url, nil)

	if err != nil {
		return nil, err
	}

	return gh.Do(r, v)
}

func ghNextPageURL(r *github.Response) string {
	if r == nil {
		return ""
	}

	if links, ok := r.Response.Header["Link"]; ok && len(links) > 0 {
		for _, link := range strings.Split(links[0], ",") {
			segments := strings.Split(strings.TrimSpace(link), ";")

			// link must at least have href and rel
			if len(segments) < 2 {
				continue
			}

			// ensure href is properly formatted
			if !strings.HasPrefix(segments[0], "<") || !strings.HasSuffix(segments[0], ">") {
				continue
			}

			url := segments[0][1 : len(segments[0])-1]

			for _, segment := range segments[1:] {
				if strings.Contains(segment, `rel="next"`) {
					return url
				}
			}
		}
	}

	return ""
}

func ReviewRequestsInOrg(connection *github.Client, org string) ([]ReviewRequest, error) {
	var allIssues []ReviewRequest
	var pageIssues []Issue
	var numIssues int

	url := fmt.Sprintf("https://api.github.com/orgs/%s/issues?filter=all&sort=updated&direction=descending", org)

	for {
		log.Info("loading org issues", "url", url)

		resp, err := ghGet(url, &pageIssues)

		if err != nil {
			log.Error("error while loading issues", "err", err)

			return nil, err
		}
		numIssues += len(pageIssues)

		for _, i := range pageIssues {
			if i.Issue.PullRequestLinks == nil {
				log.Debug("excluding non-pr issue", "issue.number", *i.Issue.Number, "url", *i.Issue.HTMLURL)
				continue
			}

			log.Debug("found pr issue", "issue.number", *i.Issue.Number, "url", *i.Issue.HTMLURL)

			allIssues = append(allIssues, ReviewRequestFromIssue(i.Repository, i.Issue, connection))
		}

		url = ghNextPageURL(resp)

		if url == "" {
			log.Info("finished loading pull request issues", "issues.len", numIssues, "pr_issues.len", len(allIssues))
			break
		}
	}

	return allIssues, nil
}

func ensureOrgReposHaveLabels(org string, client *github.Client) error {
	opts := github.RepositoryListByOrgOptions{}

	var wg sync.WaitGroup

	for {
		repos, resp, err := client.Repositories.ListByOrg(org, &opts)

		if err != nil {
			return err
		}

		for _, r := range repos {
			wg.Add(1)

			go func(r github.Repository) {
				defer wg.Done()
				log.Info("start syncing labels for repo", "repo.name", *r.Name)
				err := setupReviewFlagsInRepo(r, client)

				if err != nil {
					log.Error("error syncing repo review labels", "err", err, "repo", r.Name)
				}

				log.Info("done syncing labels for repo", "repo.name", *r.Name)
			}(r)
		}

		if resp.NextPage == 0 {
			break
		}

		opts.ListOptions.Page = resp.NextPage
	}

	wg.Wait()
	return nil

}

func setupReviewFlagsInRepo(repo github.Repository, client *github.Client) error {
	l := log.New("repo.name", repo.Name)
	opts := github.ListOptions{}
	currentLabels, _, err := client.Issues.ListLabels(*repo.Owner.Login, *repo.Name, &opts)

	if err != nil {
		l.Error("unable to fetch current labels", "err", err)
		return err
	}

	for _, label := range deprecatedLabels {
		for _, actualLabel := range currentLabels {
			if strings.ToLower(*actualLabel.Name) == strings.ToLower(label) {
				l.Info("deleting deprecated label", "repo.name", *repo.Name, "label", *actualLabel.Name)

				_, err = client.Issues.DeleteLabel(*repo.Owner.Login, *repo.Name, *actualLabel.Name)

				if err != nil {
					return err
				}
			}
		}
	}

	for label, color := range LabelColors {
		found := false
		needsUpdating := false

		for _, actualLabel := range currentLabels {
			if *actualLabel.Name == label {
				found = true

				if *actualLabel.Color != color {
					needsUpdating = true
				}

				break
			}

			if strings.ToLower(*actualLabel.Name) == strings.ToLower(label) {
				found = true
				needsUpdating = true
				break
			}
		}

		if !found {
			l.Info("creating label", "repo.Name", *repo.Name, "label.name", label, "label.color", color)

			_, _, err = client.Issues.CreateLabel(*repo.Owner.Login, *repo.Name, &github.Label{Name: &label, Color: &color})
		} else if needsUpdating {
			l.Info("updating label", "repo.Name", *repo.Name, "label.name", label, "label.color", color)

			_, _, err = client.Issues.EditLabel(*repo.Owner.Login, *repo.Name, label, &github.Label{Name: &label, Color: &color})
		}

		if err != nil {
			return err
		}
	}

	return nil
}

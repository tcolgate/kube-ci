package kubeci

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/google/go-github/v67/github"
)

type repoClient struct {
	installID int
	org       string
	repo      string

	client *github.Client
}

func (r *repoClient) GetInstallID() int {
	return r.installID
}

func (r *repoClient) GetHTTPClient() *http.Client {
	return r.client.Client()
}

func (r *repoClient) GetRef(ctx context.Context, ref string) (*github.Reference, error) {
	gref, _, err := r.client.Git.GetRef(
		ctx,
		r.org,
		r.repo,
		ref,
	)
	return gref, err
}

func (r *repoClient) GetRepo(ctx context.Context) (*github.Repository, error) {
	res, _, err := r.client.Repositories.Get(
		ctx,
		r.org,
		r.repo,
	)
	return res, err
}

func (r *repoClient) UpdateCheckRun(ctx context.Context, id int64, upd github.UpdateCheckRunOptions) (*github.CheckRun, error) {
	cr, _, err := r.client.Checks.UpdateCheckRun(
		ctx,
		r.org,
		r.repo,
		id,
		upd,
	)
	return cr, err
}

func StatusUpdate(
	ctx context.Context,
	info *githubInfo,
	status GithubStatus,
) {
	opts := github.UpdateCheckRunOptions{
		// Mandatory on update
		Name: info.checkRunName,
	}

	// These are optional
	if status.DetailsURL != "" {
		opts.DetailsURL = &status.DetailsURL
	}

	if status.Status != "" {
		opts.Status = &status.Status
	}

	if status.Conclusion != "" {
		opts.Conclusion = &status.Conclusion
		opts.CompletedAt = &github.Timestamp{
			Time: time.Now(),
		}
	}

	// If any of these are set we update the output,
	// so all must be set
	if status.Title != "" ||
		status.Summary != "" ||
		status.Text != "" {
		opts.Output = &github.CheckRunOutput{
			Title:       &status.Title,
			Summary:     &status.Summary,
			Text:        &status.Text,
			Annotations: status.Annotations,
		}
	}

	opts.Actions = status.Actions

	_, err := info.ghClient.UpdateCheckRun(
		ctx,
		info.checkRunID,
		opts)

	if err != nil {
		log.Printf("Update of aborted check run failed, %v", err)
	}
}

func (r *repoClient) CreateCheckRun(ctx context.Context, opts github.CreateCheckRunOptions) (*github.CheckRun, error) {
	cr, _, err := r.client.Checks.CreateCheckRun(ctx,
		r.org,
		r.repo,
		opts,
	)
	return cr, err
}

func (r *repoClient) CreateDeployment(ctx context.Context, req *github.DeploymentRequest) (*github.Deployment, error) {
	dep, _, err := r.client.Repositories.CreateDeployment(
		ctx,
		r.org,
		r.repo,
		req,
	)
	return dep, err
}

func (r *repoClient) CreateDeploymentStatus(ctx context.Context, id int64, req *github.DeploymentStatusRequest) (*github.DeploymentStatus, error) {
	dep, _, err := r.client.Repositories.CreateDeploymentStatus(
		ctx,
		r.org,
		r.repo,
		id,
		req,
	)
	return dep, err
}

func (r *repoClient) IsMember(ctx context.Context, user string) (bool, error) {
	ok, _, err := r.client.Organizations.IsMember(
		ctx,
		r.org,
		user,
	)
	return ok, err
}

func (r *repoClient) GetContents(ctx context.Context, dirpath string, opts *github.RepositoryContentGetOptions) ([]*github.RepositoryContent, error) {
	var out []*github.RepositoryContent

	_, dir, resp, err := r.client.Repositories.GetContents(
		ctx,
		r.org,
		r.repo,
		dirpath,
		opts,
	)

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s not found in repo, %w", dirpath, os.ErrNotExist)
	}

	for _, f := range dir {
		file, _, _, ferr := r.client.Repositories.GetContents(
			ctx,
			r.org,
			r.repo,
			f.GetPath(),
			opts,
		)
		if ferr != nil {
			return nil, ferr
		}
		out = append(out, file)
	}

	return out, err
}

func (r *repoClient) CreateFile(ctx context.Context, filepath string, opts *github.RepositoryContentFileOptions) error {
	_, _, err := r.client.Repositories.CreateFile(ctx, r.org, r.repo, filepath, opts)
	return err
}

func (r *repoClient) GetBranch(ctx context.Context, branch string) (*github.Branch, error) {
	gbranch, _, err := r.client.Repositories.GetBranch(ctx, r.org, r.repo, branch, 4)
	return gbranch, err
}

func (r *repoClient) GetPullRequest(ctx context.Context, prid int) (*github.PullRequest, error) {
	pr, _, err := r.client.PullRequests.Get(ctx, r.org, r.repo, prid)
	return pr, err
}

func (r *repoClient) CreateIssueComment(ctx context.Context, issueID int, opts *github.IssueComment) error {
	_, _, err := r.client.Issues.CreateComment(
		ctx,
		r.org,
		r.repo,
		issueID,
		opts)
	return err
}

type contentDownloader interface {
	GetContents(ctx context.Context, filepath string, opts *github.RepositoryContentGetOptions) ([]*github.RepositoryContent, error)
}

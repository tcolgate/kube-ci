// Copyright 2019 Qubit Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

// controller.go: the controller (TODO: also currently syncer), updates
// a github check run to match the current state of a workflow created
// in kubernetes.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	workflow "github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	clientset "github.com/argoproj/argo/pkg/client/clientset/versioned"
	informers "github.com/argoproj/argo/pkg/client/informers/externalversions"
	listers "github.com/argoproj/argo/pkg/client/listers/workflow/v1alpha1"

	"github.com/bradleyfalzon/ghinstallation"
	"github.com/google/go-github/v32/github"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

var (
	annCommit               = "kube-ci.qutics.com/sha"
	annBranch               = "kube-ci.qutics.com/branch"
	annRepo                 = "kube-ci.qutics.com/repo"
	annOrg                  = "kube-ci.qutics.com/org"
	annInstID               = "kube-ci.qutics.com/github-install-id"
	annCheckRunName         = "kube-ci.qutics.com/check-run-name"
	annCheckRunID           = "kube-ci.qutics.com/check-run-id"
	annAnnotationsPublished = "kube-ci.qutics.com/annotations-published"

	annCacheVolumeName             = "kube-ci.qutics.com/cacheName"
	annCacheVolumeScope            = "kube-ci.qutics.com/cacheScope"
	annCacheVolumeStorageSize      = "kube-ci.qutics.com/cacheSize"
	annCacheVolumeStorageClassName = "kube-ci.qutics.com/cacheStorageClassName"

	annRunBranch = "kube-ci.qutics.com/runForBranch"
	annRunTag    = "kube-ci.qutics.com/runForTag"

	labelManagedBy = "managedBy"
	labelWFType    = "wfType"
	labelOrg       = "org"
	labelRepo      = "repo"
	labelBranch    = "branch"
	labelScope     = "scope"
)

type githubKeyStore struct {
	baseTransport http.RoundTripper
	appID         int64
	ids           []int
	key           []byte
	orgs          *regexp.Regexp
}

func (ks *githubKeyStore) getClient(org string, installID int, repo, owner string) (*repoClient, error) {
	validID := false
	for _, id := range ks.ids {
		if installID == id {
			validID = true
			break
		}
	}

	if len(ks.ids) > 0 && !validID {
		return nil, fmt.Errorf("unknown installation %d", installID)
	}

	if ks.orgs != nil && !ks.orgs.MatchString(org) {
		return nil, fmt.Errorf("refusing event from untrusted org %s", org)
	}

	itr, err := ghinstallation.New(ks.baseTransport, ks.appID, int64(installID), ks.key)
	if err != nil {
		return nil, err
	}

	ghc, err := github.NewClient(&http.Client{Transport: itr}), nil
	if err != nil {
		return nil, err
	}

	return &repoClient{
		installID: installID,
		org:       org,
		client:    ghc,
		repo:      repo,
		owner:     owner,
	}, nil
}

type repoClient struct {
	installID int
	org       string
	repo      string
	owner     string

	client *github.Client
}

func (r *repoClient) GetInstallID() int {
	return r.installID
}

func (r *repoClient) GetRef(ctx context.Context, ref string) (*github.Reference, error) {
	gref, _, err := r.client.Git.GetRef(
		ctx,
		r.owner,
		r.repo,
		ref,
	)
	return gref, err
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

func (r *repoClient) StatusUpdate(
	ctx context.Context,
	crID int64,
	title string,
	msg string,
	status string,
	conclusion string,
) {
	log.Print(msg)
	opts := github.UpdateCheckRunOptions{
		Name:   checkRunName,
		Status: &status,
		Output: &github.CheckRunOutput{
			Title:   &title,
			Summary: &msg,
		},
	}

	if conclusion != "" {
		opts.Conclusion = &conclusion
		opts.CompletedAt = &github.Timestamp{
			Time: time.Now(),
		}
	}
	_, _, err := r.client.Checks.UpdateCheckRun(
		ctx,
		r.org,
		r.repo,
		crID,
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

func (r *repoClient) DownloadContents(ctx context.Context, filepath string, opts *github.RepositoryContentGetOptions) (io.ReadCloser, error) {
	return r.client.Repositories.DownloadContents(
		ctx,
		r.org,
		r.repo,
		filepath,
		opts,
	)
}

func (r *repoClient) GetContents(ctx context.Context, filepath string, opts *github.RepositoryContentGetOptions) ([]*github.RepositoryContent, error) {
	_, files, _, err := r.client.Repositories.GetContents(
		ctx,
		r.org,
		r.repo,
		filepath,
		opts,
	)
	return files, err
}

func (r *repoClient) CreateFile(ctx context.Context, filepath string, opts *github.RepositoryContentFileOptions) error {
	_, _, err := r.client.Repositories.CreateFile(ctx, r.org, r.repo, filepath, opts)
	return err
}

func (r *repoClient) GetBranch(ctx context.Context, branch string) (*github.Branch, error) {
	gbranch, _, err := r.client.Repositories.GetBranch(ctx, r.org, r.repo, branch)
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

type githubClientSource interface {
	getClient(org string, installID int, repo, owner string) (*repoClient, error)
}

// CacheSpec lets you choose the default settings for a
// per-job cache volume.
type CacheSpec struct {
	Scope            string `yaml:"scope"`
	Size             string `yaml:"size"`
	StorageClassName string `yaml:"storageClassName"`
}

// TemplateSpec gives the description, and location, of a set
// of config files for use by the setup slash command
type TemplateSpec struct {
	Description string `yaml:"description"`
	CI          string `yaml:"ci"`
	Deploy      string `yaml:"deploy"`
}

// TemplateSet describes a set of templates
type TemplateSet map[string]TemplateSpec

// Config defines our configuration file format
type Config struct {
	CIFilePath    string            `yaml:"ciFilePath"`
	Namespace     string            `yaml:"namespace"`
	Tolerations   []v1.Toleration   `yaml:"tolerations"`
	NodeSelector  map[string]string `yaml:"nodeSelector"`
	TemplateSet   TemplateSet       `yaml:"templates"`
	CacheDefaults CacheSpec         `yaml:"cacheDefaults"`
	BuildDraftPRs bool              `yaml:"buildDraftPRs"`
	BuildBranches string            `yaml:"buildBranches"`

	buildBranches *regexp.Regexp
}

type workflowSyncer struct {
	appID    int64
	ghSecret []byte

	ghClientSrc githubClientSource

	config     Config
	kubeclient kubernetes.Interface
	client     clientset.Interface
	lister     listers.WorkflowLister
	synced     cache.InformerSynced
	workqueue  workqueue.RateLimitingInterface

	argoUIBase string
}

var sanitize = regexp.MustCompile(`[^-a-z0-9]`)
var sanitizeToDNS = regexp.MustCompile(`^[-.0-9]*`)

func escape(str string) string {
	return sanitize.ReplaceAllString(strings.ToLower(str), "-")
}

func labelSafe(strs ...string) string {
	escStrs := make([]string, len(strs))
	for i := 0; i < len(strs); i++ {
		escStrs[i] = escape(strs[i])
	}

	str := strings.Join(escStrs, ".")

	maxLen := 50
	if len(str) > maxLen {
		strOver := maxLen - len(str)
		str = str[strOver*-1:]
	}

	return sanitizeToDNS.ReplaceAllString(str, "")
}

func (ws *workflowSyncer) enqueue(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	ws.workqueue.AddRateLimited(key)
}

func (ws *workflowSyncer) doDelete(obj interface{}) {
	// we want to update the check run status for any
	// workflows that are deleted while still
	wf, ok := obj.(*workflow.Workflow)
	if !ok {
		return
	}

	cri, err := crInfoFromWorkflow(wf)
	if err != nil {
		return
	}

	if !wf.Status.FinishedAt.IsZero() {
		return
	}

	ghClient, err := ws.ghClientSrc.getClient(cri.orgName, int(cri.instID), cri.repoName, "OWNER")
	if err != nil {
		return
	}

	status := "completed"
	conclusion := "cancelled"
	ghClient.UpdateCheckRun(
		context.Background(),
		cri.checkRunID,
		github.UpdateCheckRunOptions{
			Name:        cri.checkRunName,
			HeadSHA:     &cri.headSHA,
			Status:      &status,
			Conclusion:  &conclusion,
			CompletedAt: &github.Timestamp{Time: time.Now()},
		},
	)
}

func newWorkflowSyncer(
	kubeclient kubernetes.Interface,
	clientset clientset.Interface,
	sinf informers.SharedInformerFactory,
	ghClientSrc githubClientSource,
	appID int64,
	ghSecret []byte,
	baseURL string,
	config Config,
) *workflowSyncer {

	informer := sinf.Argoproj().V1alpha1().Workflows()

	syncer := &workflowSyncer{
		appID:       appID,
		ghClientSrc: ghClientSrc,
		ghSecret:    ghSecret,

		kubeclient: kubeclient,
		client:     clientset,
		lister:     informer.Lister(),
		synced:     informer.Informer().HasSynced,
		workqueue:  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "workflows"),
		argoUIBase: baseURL,
		config:     config,
	}

	log.Print("Setting up event handlers")
	informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: syncer.enqueue,
		UpdateFunc: func(old, new interface{}) {
			syncer.enqueue(new)
		},
		DeleteFunc: syncer.doDelete,
	})

	return syncer
}

func (ws *workflowSyncer) runWorker() {
	for ws.process() {
	}

	log.Print("worker stopped")
}

func (ws *workflowSyncer) process() bool {
	obj, shutdown := ws.workqueue.Get()

	if shutdown {
		return false
	}

	defer ws.workqueue.Done(obj)

	var key string
	var ok bool
	if key, ok = obj.(string); !ok {
		ws.workqueue.Forget(obj)
		runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
		return true
	}

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		ws.workqueue.Forget(obj)
		runtime.HandleError(fmt.Errorf("couldn't split workflow cache key %q, %v", key, err))
		return true
	}

	wf, err := ws.lister.Workflows(namespace).Get(name)
	if err != nil {
		ws.workqueue.Forget(obj)
		runtime.HandleError(fmt.Errorf("couldn't get workflow %q, %v ", key, err))
		return true
	}

	err = ws.sync(wf)
	if err != nil {
		runtime.HandleError(err)
		return true
	}

	ws.workqueue.Forget(obj)

	return true
}

type crInfo struct {
	orgName  string
	repoName string
	instID   int

	headSHA    string
	headBranch string

	checkRunName string
	checkRunID   int64
}

func crInfoFromWorkflow(wf *workflow.Workflow) (*crInfo, error) {
	instIDStr, ok := wf.Annotations[annInstID]
	if !ok {
		return nil, fmt.Errorf("could not get github installation id for  %s/%s", wf.Namespace, wf.Name)
	}

	instID, err := strconv.Atoi(instIDStr)
	if err != nil {
		return nil, fmt.Errorf("could not convert installation id for %s/%s to int", wf.Namespace, wf.Name)
	}

	headSHA, ok := wf.Annotations[annCommit]
	if !ok {
		return nil, fmt.Errorf("could not get commit sha for %s/%s", wf.Namespace, wf.Name)
	}
	headBranch, ok := wf.Annotations[annBranch]
	if !ok {
		return nil, fmt.Errorf("could not get commit branch for %s/%s", wf.Namespace, wf.Name)
	}
	orgName, ok := wf.Annotations[annOrg]
	if !ok {
		return nil, fmt.Errorf("could not get github org for %s/%s", wf.Namespace, wf.Name)
	}
	repoName, ok := wf.Annotations[annRepo]
	if !ok {
		return nil, fmt.Errorf("could not get github repo name for %s/%s", wf.Namespace, wf.Name)
	}
	checkRunName, ok := wf.Annotations[annCheckRunName]
	if !ok {
		return nil, fmt.Errorf("could not get check run name for %s/%s", wf.Namespace, wf.Name)
	}

	checkRunIDStr, ok := wf.Annotations[annCheckRunID]
	if !ok {
		return nil, fmt.Errorf("could not get check run id for  %s/%s", wf.Namespace, wf.Name)
	}
	checkRunID, err := strconv.Atoi(checkRunIDStr)
	if err != nil {
		return nil, fmt.Errorf("could not convert check  run id for %s/%s to int", wf.Namespace, wf.Name)
	}

	return &crInfo{
		headSHA:      headSHA,
		instID:       instID,
		headBranch:   headBranch,
		orgName:      orgName,
		repoName:     repoName,
		checkRunName: checkRunName,
		checkRunID:   int64(checkRunID),
	}, nil
}

func (ws *workflowSyncer) resetCheckRun(wf *workflow.Workflow) (*workflow.Workflow, error) {
	newWf := wf.DeepCopy()

	cr, err := crInfoFromWorkflow(wf)
	if err != nil {
		return nil, fmt.Errorf("no check-run info found in restarted workflow (%s/%s)", wf.Namespace, wf.Name)
	}

	ghClient, err := ws.ghClientSrc.getClient(cr.orgName, int(cr.instID), cr.repoName, "OWNER")
	if err != nil {
		return nil, err
	}

	newCR, err := ghClient.CreateCheckRun(context.TODO(),
		github.CreateCheckRunOptions{
			Name:    checkRunName,
			HeadSHA: cr.headSHA,
			Status:  initialCheckRunStatus,
			Output: &github.CheckRunOutput{
				Title:   github.String("Workflow Setup"),
				Summary: github.String("Creating workflow"),
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed creating new check run, %w", err)
	}

	/*
		repo := &github.Repository{
			Owner: &github.User{
				Login: github.String(cr.orgName),
			},
			Name: github.String(cr.repoName),
		}
	*/

	ghClient.StatusUpdate(
		context.Background(),
		*newCR.ID,
		"Workflow Setup",
		"Creating workflow",
		"queued",
		"",
	)

	newWf.Annotations[annAnnotationsPublished] = "false"
	newWf.Annotations[annCheckRunName] = newCR.GetName()
	newWf.Annotations[annCheckRunID] = strconv.Itoa(int(newCR.GetID()))

	return ws.client.ArgoprojV1alpha1().Workflows(newWf.GetNamespace()).Update(newWf)
}

func (ws *workflowSyncer) sync(wf *workflow.Workflow) error {
	var err error

	log.Printf("got workflow phase: %v/%v %v", wf.Namespace, wf.Name, wf.Status.Phase)

	if v, ok := wf.Annotations[annAnnotationsPublished]; ok && v == "true" {
		switch wf.Status.Phase {
		case workflow.NodePending, workflow.NodeRunning: // attempt create new checkrun for a resubmitted job
			wf, err = ws.resetCheckRun(wf)
			if err != nil {
				log.Printf("failed checkrun reset, %v", err)
				return nil
			}
		default:
			// The workflow is not yet running, ignore it
			return nil
		}
	}

	cr, err := crInfoFromWorkflow(wf)
	if err != nil {
		log.Printf("ignoring %s/%s, %v", wf.Namespace, wf.Name, err)
		return nil
	}

	ghClient, err := ws.ghClientSrc.getClient(cr.orgName, int(cr.instID), cr.repoName, "OWNER")
	if err != nil {
		return err
	}

	status := ""

	failure := "failure"
	success := "success"
	neutral := "neutral"
	cancelled := "cancelled"
	var conclusion *string

	var completedAt *github.Timestamp
	now := &github.Timestamp{
		Time: time.Now(),
	}

	switch wf.Status.Phase {
	case workflow.NodePending:
		status = *initialCheckRunStatus
	case workflow.NodeRunning:
		status = "in_progress"
	case workflow.NodeFailed:
		status = "completed"
		conclusion = &failure
		if wf.Spec.ActiveDeadlineSeconds != nil && *wf.Spec.ActiveDeadlineSeconds == 0 {
			conclusion = &cancelled
		}
		completedAt = now
	case workflow.NodeError:
		status = "completed"
		conclusion = &failure
		completedAt = now
	case workflow.NodeSucceeded:
		status = "completed"
		conclusion = &success
		completedAt = now
	case workflow.NodeSkipped:
		status = "completed"
		conclusion = &neutral
		completedAt = now
	default:
		log.Printf("ignoring %s/%s, unknown node phase %q", wf.Namespace, wf.Name, wf.Status.Phase)
		return nil
	}

	summary := wf.Status.Message

	title := fmt.Sprintf("Workflow Run (%s/%s))", wf.Namespace, wf.Name)
	text := ""
	var names []string
	namesToNodes := make(map[string]string)
	for k, v := range wf.Status.Nodes {
		if v.Type != "Pod" {
			continue
		}
		names = append(names, v.Name)
		namesToNodes[v.Name] = k
	}
	sort.Strings(names)
	for _, k := range names {
		n := namesToNodes[k]
		node := wf.Status.Nodes[n]
		text += fmt.Sprintf("%s(%s): %s \n", k, node.Phase, node.Message)
	}

	wfURL := fmt.Sprintf(
		"%s/workflows/%s/%s",
		ws.argoUIBase,
		wf.Namespace,
		wf.Name)

	_, err = ghClient.UpdateCheckRun(
		context.Background(),
		cr.checkRunID,
		github.UpdateCheckRunOptions{
			Name:        cr.checkRunName,
			HeadSHA:     &cr.headSHA,
			DetailsURL:  &wfURL,
			Status:      &status,
			Conclusion:  conclusion,
			CompletedAt: completedAt,

			Output: &github.CheckRunOutput{
				Title:   &title,
				Summary: &summary,
				Text:    &text,
			},
		},
	)

	if err != nil {
		log.Printf("Unable to update check run, %v", err)
	}

	if status == "completed" {
		go ws.completeCheckRun(&title, &summary, &text, wf, cr)
	}

	return nil
}

// completeCheckRun is used to publish any annotations found in the logs from a check run.
// There are a bunch of reasons this could fail.
func (ws *workflowSyncer) completeCheckRun(title, summary, text *string, wf *workflow.Workflow, cri *crInfo) {
	if wf.Status.Phase != workflow.NodeFailed &&
		wf.Status.Phase != workflow.NodeSucceeded {
		return
	}

	var allAnns []*github.CheckRunAnnotation
	for _, n := range wf.Status.Nodes {
		if n.Type != "Pod" {
			continue
		}
		logr, err := getPodLogs(ws.kubeclient, n.ID, wf.Namespace, "main")
		if err != nil {
			log.Printf("getting pod logs failed, %v", err)
			continue
		}
		anns, err := parseAnnotations(logr, "")
		if err != nil {
			log.Printf("parsing annotations failed, %v", err)
			return
		}
		allAnns = append(allAnns, anns...)
	}

	ghClient, err := ws.ghClientSrc.getClient(cri.orgName, int(cri.instID), cri.repoName, "OWNER")
	if err != nil {
		return
	}

	var actions []*github.CheckRunAction
	/*
		if wf.Status.Phase == workflow.NodeSucceeded {
				actions = []*github.CheckRunAction{
					{
						Label:       "Deploy Me",
						Description: "Do the thing ",
						Identifier:  "deploy",
					},
				}
		}
	*/

	_, err = ghClient.UpdateCheckRun(
		context.Background(),
		cri.checkRunID,
		github.UpdateCheckRunOptions{
			Name:    cri.checkRunName,
			HeadSHA: &cri.headSHA,
			Actions: actions,
		},
	)
	if err != nil {
		log.Printf("error, failed updating check run status, %v", err)
	}

	batchSize := 50 // github API allows 50 at a time
	for i := 0; i < len(allAnns); i += batchSize {
		start := i
		end := i + batchSize
		if end > len(allAnns) {
			end = len(allAnns)
		}
		anns := allAnns[start:end]
		_, err = ghClient.UpdateCheckRun(
			context.Background(),
			cri.checkRunID,
			github.UpdateCheckRunOptions{
				Name:    cri.checkRunName,
				HeadSHA: &cri.headSHA,

				Output: &github.CheckRunOutput{
					Title:       title,
					Summary:     summary,
					Text:        text,
					Annotations: anns,
				},
				Actions: actions,
			},
		)
		if err != nil {
			log.Printf("upload annotations for %s/%s failed, %v", wf.Namespace, wf.Name, err)

		}
	}

	// We need to update the API object so that we know we've published the
	// logs, we'll grab the latest one incase it has changed since we got here.
	newwf, err := ws.client.ArgoprojV1alpha1().Workflows(ws.config.Namespace).Get(wf.Name, metav1.GetOptions{})
	if err != nil {
		log.Printf("getting workflow %s/%s for annotations update failed, %v", newwf.Namespace, newwf.Name, err)
		return
	}

	upwf := newwf.DeepCopy()
	if upwf.Annotations == nil {
		upwf.Annotations = map[string]string{}
	}
	upwf.Annotations[annAnnotationsPublished] = "true"

	_, err = ws.client.ArgoprojV1alpha1().Workflows(ws.config.Namespace).Update(upwf)
	if err != nil {
		log.Printf("workflow %s/%s update for annotations update failed, %v", upwf.Namespace, upwf.Name, err)
	}
}

func (ws *workflowSyncer) Run(stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer ws.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, ws.synced); !ok {
		log.Printf("failed waiting for cache sync")
		return fmt.Errorf("caches did not sync")
	}

	go wait.Until(ws.runWorker, time.Second, stopCh)

	log.Print("Started workers")
	<-stopCh
	log.Print("Shutting down workers")

	return nil
}

func (ws *workflowSyncer) getFile(
	ctx context.Context,
	ghClient *repoClient,
	owner string,
	name string,
	sha string,
	filename string) (io.ReadCloser, error) {

	file, err := ghClient.DownloadContents(
		ctx,
		filename,
		&github.RepositoryContentGetOptions{
			Ref: sha,
		})
	if err != nil {
		if ghErr, ok := err.(*github.ErrorResponse); ok {
			if ghErr.Response.StatusCode == http.StatusNotFound {
				return nil, os.ErrNotExist
			}
		}
		return nil, err
	}
	return file, nil
}

func (ws *workflowSyncer) getWorkflow(
	ctx context.Context,
	ghClient *repoClient,
	repo *github.Repository,
	sha string,
	filename string) (*workflow.Workflow, error) {

	file, err := ws.getFile(
		ctx,
		ghClient,
		repo.GetOwner().GetLogin(),
		repo.GetName(),
		sha,
		filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	bs := &bytes.Buffer{}

	_, err = io.Copy(bs, file)
	if err != nil {
		return nil, err
	}

	decode := scheme.Codecs.UniversalDeserializer().Decode
	obj, _, err := decode(bs.Bytes(), nil, nil)

	if err != nil {
		return nil, fmt.Errorf("failed to decode %s, %v", filename, err)
	}

	wf, ok := obj.(*workflow.Workflow)
	if !ok {
		return nil, fmt.Errorf("could not use %T as workflow", wf)
	}

	return wf, nil
}

// Copyright 2017 The Bazel Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cistarlark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/google/go-github/v67/github"
	"go.starlark.net/starlark"
	"k8s.io/client-go/kubernetes/scheme"

	starlibyaml "github.com/qri-io/starlib/encoding/yaml"
	starlibre "github.com/qri-io/starlib/re"
	starlibjson "go.starlark.net/lib/json"
	starlibmath "go.starlark.net/lib/math"
	starlibtime "go.starlark.net/lib/time"
	"go.starlark.net/starlarkstruct"

	workflow "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
)

type githubContentGetter interface {
	GetContents(ctx context.Context, owner, repo, path string, opts *github.RepositoryContentGetOptions) (fileContent *github.RepositoryContent, directoryContent []*github.RepositoryContent, resp *github.Response, err error)
}

type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type modOpener interface {
	ModOpen(*starlark.Thread, string) (starlark.StringDict, error)
}

var (
	emptyStr = starlark.String("")
)

func setCWU(thread *starlark.Thread, u *url.URL) {
	thread.SetLocal(workingURL, u)
}

func getCWU(thread *starlark.Thread) *url.URL {
	ui := thread.Local(workingURL)
	if ui == nil {
		thread.Cancel("working URL not set")
		return nil
	}
	return ui.(*url.URL)
}

type modSrc struct {
	builtIn     map[string]starlark.StringDict
	predeclared starlark.StringDict
	context     map[string]string

	client githubContentGetter
	http   httpDoer
}

func newModSource(http *http.Client) *modSrc {

	ghc := github.NewClient(http)

	return &modSrc{
		client: ghc.Repositories,
		http:   http,
	}
}

func (gh *modSrc) SetBuiltIn(builtIn map[string]starlark.StringDict) {
	gh.builtIn = builtIn
}

func (gh *modSrc) SetPredeclared(predeclared starlark.StringDict) {
	gh.predeclared = predeclared
}

func (gh *modSrc) SetContext(cntx map[string]string) {
	gh.context = cntx
}

func validateModURL(u *url.URL) error {
	var err error
	switch u.Scheme {
	case "github":
		if len(strings.Split(u.Host, ".")) != 2 {
			return fmt.Errorf("invalid host %q for github scheme, should be empty, repo, or org.repo", u.Host)
		}
	case "builtin":
		if u.Host != "" {
			return fmt.Errorf("host is not invalid for the builtin scheme")
		}
	case "context":
		if u.Host != "" {
			return fmt.Errorf("host is not invalid for the context scheme")
		}
	case "http":
	}

	return err
}

func (gh *modSrc) builtInLoadFile(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var fn string
	var errOnNotFound = starlark.Bool(true)

	err := starlark.UnpackArgs(b.Name(), args, kwargs,
		"fn", &fn,
		"error_on_notfound?", &errOnNotFound)
	if err != nil {
		return nil, err
	}

	cwu := getCWU(thread)
	u, err := parseModURL(cwu, fn)
	if err != nil {
		return nil, err
	}

	var res starlark.String
	loaders := map[string]func(*starlark.Thread, *url.URL) (starlark.String, error){
		"github":  gh.openGithubFile,
		"https":   gh.openHTTPFile,
		"context": gh.openContextFile,
	}

	loader, ok := loaders[u.Scheme]
	if !ok {
		return nil, fmt.Errorf("unsupported scheme, %s", u.Scheme)
	}

	res, err = loader(thread, u)
	if err != nil {
		if !bool(errOnNotFound) && errors.Is(err, os.ErrNotExist) {
			return starlark.None, nil
		}
		return nil, fmt.Errorf("could not open file %s, %w", u, err)
	}
	return res, nil
}

// Open loads content from git, you can use
// "builtin:///module" - load a kube-ci built in
// "file.star" -  from your .kube-ci directory, current ref
// "/file.star" -  from your the root of your repo, current ref
// "https://somehost/file.star" -  (bad idea?
// "github:///file.star" -  from your .kube-ci directory, current ref
// "github://repo/file.star" -  from a repo in your org, absolute from your
// "github://repo.org/file.star?ref=something" -  absolute from your .kube-ci directory, current ref
func parseModURL(base *url.URL, name string) (*url.URL, error) {
	defaultRef := base.Query().Get("ref")

	var defaultOrg string
	if base.Scheme == "github" {
		parts := strings.SplitN(base.Host, ".", 2)
		defaultOrg = parts[1]
	}

	u, err := base.Parse(name)
	if err != nil {
		return nil, err
	}

	if u.Scheme == "github" {
		qs := u.Query()
		if u.Scheme == base.Scheme {
			if len(strings.Split(u.Host, ".")) == 1 {
				u.Host = fmt.Sprintf("%s.%s", u.Host, defaultOrg)
			}

			if qs.Get("ref") == "" {
				qs.Set("ref", defaultRef)
				if u.Host != base.Host {
					qs.Set("ref", "")
				}
				u.RawQuery = qs.Encode()
			}
		} else {
			qs.Set("ref", "")
			u.RawQuery = qs.Encode()
		}
	}

	if err := validateModURL(u); err != nil {
		return nil, err
	}

	return u, nil
}

func (gh *modSrc) openContextFile(thread *starlark.Thread, url *url.URL) (starlark.String, error) {
	if gh.context == nil {
		return emptyStr, fmt.Errorf("file not found in context")
	}

	str, ok := gh.context[strings.TrimPrefix(url.Path, "/")]
	if !ok {
		return emptyStr, fmt.Errorf("not in context, %w", os.ErrNotExist)
	}
	return starlark.String(str), nil
}

func (gh *modSrc) openContext(thread *starlark.Thread, url *url.URL) (starlark.StringDict, error) {
	str, err := gh.openContextFile(thread, url)
	if err != nil {
		return nil, fmt.Errorf("reading context data failed, %w", err)
	}

	return starlark.ExecFile(thread, url.String(), string(str), gh.predeclared)
}

func (gh *modSrc) openHTTPFile(thread *starlark.Thread, url *url.URL) (starlark.String, error) {
	req, err := http.NewRequest(http.MethodGet, url.String(), nil)
	if err != nil {
		return emptyStr, fmt.Errorf("invalid HTTP request, %w", err)
	}
	resp, err := gh.http.Do(req)
	if err != nil {
		return emptyStr, err
	}
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("invalid HTTP status code, %v", resp.StatusCode)
		if resp.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("%s, %w", err, os.ErrNotExist)
		}
		return emptyStr, err
	}

	bs, err := io.ReadAll(resp.Body)
	if err != nil {
		return emptyStr, fmt.Errorf("reading HTTP body failed, %w", err)
	}
	return starlark.String(string(bs)), nil
}

func (gh *modSrc) openHTTP(thread *starlark.Thread, url *url.URL) (starlark.StringDict, error) {
	str, err := gh.openHTTPFile(thread, url)
	if err != nil {
		return nil, fmt.Errorf("reading HTTP body failed, %w", err)
	}

	return starlark.ExecFile(thread, url.String(), string(str), gh.predeclared)
}

func (gh *modSrc) openGithubFile(thread *starlark.Thread, url *url.URL) (starlark.String, error) {
	ctx := context.Background()

	// this has already been validated
	repo, org, _ := strings.Cut(url.Host, ".")
	ref := url.Query().Get("ref")

	file, _, resp, err := gh.client.GetContents(
		ctx,
		org,
		repo,
		url.Path,
		&github.RepositoryContentGetOptions{
			Ref: ref,
		},
	)
	if err != nil {
		if resp.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("%s, %w", err, os.ErrNotExist)
		}
		return emptyStr, err
	}

	str, err := file.GetContent()
	if err != nil {
		return emptyStr, err
	}

	return starlark.String(str), nil
}

func (gh *modSrc) openGithub(thread *starlark.Thread, url *url.URL) (starlark.StringDict, error) {
	str, err := gh.openGithubFile(thread, url)
	if err != nil {
		return nil, err
	}

	mod, err := starlark.ExecFile(thread, url.String(), string(str), gh.predeclared)

	return mod, err
}

func (gh *modSrc) openBuiltin(thread *starlark.Thread, u *url.URL) (starlark.StringDict, error) {
	mod, ok := gh.builtIn[u.Path]
	if !ok {
		return nil, fmt.Errorf("unknown builtin module, %s", u.Path)
	}
	return mod, nil
}

func (gh *modSrc) ModOpen(thread *starlark.Thread, name string) (starlark.StringDict, error) {
	cwu := getCWU(thread)
	u, err := parseModURL(cwu, name)
	if err != nil {
		return nil, err
	}

	var mod starlark.StringDict
	loaders := map[string]func(*starlark.Thread, *url.URL) (starlark.StringDict, error){
		"github":  gh.openGithub,
		"https":   gh.openHTTP,
		"builtin": gh.openBuiltin,
		"context": gh.openContext,
	}

	loader, ok := loaders[u.Scheme]
	if !ok {
		return nil, fmt.Errorf("unsupported scheme, %s", u.Scheme)
	}

	mod, err = loader(thread, u)
	if err != nil {
		return nil, fmt.Errorf("could not open module %s, %w", u, err)
	}
	return mod, nil
}

// modCache is a concurrency-safe, duplicate-suppressing,
// non-blocking modCache of the doLoad function.
// See Section 9.7 of gopl.io for an explanation of this structure.
// It also features online deadlock (load cycle) detection.
type modCache struct {
	modCacheMu sync.Mutex
	modCache   map[string]*entry

	src     modOpener
	starLog func(thread *starlark.Thread, msg string)
}

type entry struct {
	owner   unsafe.Pointer // a *cycleChecker; see cycleCheck
	globals starlark.StringDict
	err     error
	ready   chan struct{}
}

var workingURL = "CWU"

func resolveModuleName(thread *starlark.Thread, module string) *url.URL {
	cwu := getCWU(thread)

	nwu, err := cwu.Parse(module)
	if err != nil {
		thread.Cancel(err.Error())
	}
	if nwu.RawQuery == "" {
		nwu.RawQuery = cwu.RawQuery
	}
	return nwu
}

func (c *modCache) doLoad(_ *starlark.Thread, cc *cycleChecker, modurl *url.URL) (starlark.StringDict, error) {
	thread := &starlark.Thread{
		Name:  "exec " + modurl.String(),
		Print: c.starLog,
		Load: func(thread *starlark.Thread, module string) (starlark.StringDict, error) {
			modURL := resolveModuleName(thread, module)
			return c.get(thread, cc, modURL)
		},
	}

	setCWU(thread, modurl)
	mod, err := c.src.ModOpen(thread, modurl.String())
	if err != nil {
		return nil, err
	}

	return mod, nil
}

// get loads and returns an entry (if not already loaded).
func (c *modCache) get(thread *starlark.Thread, cc *cycleChecker, module *url.URL) (starlark.StringDict, error) {
	moduleName := module.String()

	c.modCacheMu.Lock()
	e := c.modCache[moduleName]
	if e != nil {
		c.modCacheMu.Unlock()
		// Some other goroutine is getting this module.
		// Wait for it to become ready.

		// Detect load cycles to avoid deadlocks.
		if err := cycleCheck(e, cc); err != nil {
			return nil, err
		}

		cc.setWaitsFor(e)
		<-e.ready
		cc.setWaitsFor(nil)
	} else {
		// First request for this module.
		e = &entry{ready: make(chan struct{})}
		c.modCache[moduleName] = e
		c.modCacheMu.Unlock()

		e.setOwner(cc)
		e.globals, e.err = c.doLoad(thread, cc, module)
		e.setOwner(nil)

		// Broadcast that the entry is now ready.
		close(e.ready)
	}
	return e.globals, e.err
}

func (c *modCache) Load(thread *starlark.Thread, module string) (starlark.StringDict, error) {
	modURL := resolveModuleName(thread, module)
	return c.get(thread, new(cycleChecker), modURL)
}

// A cycleChecker is used for concurrent deadlock detection.
// Each top-level call to Load creates its own cycleChecker,
// which is passed to all recursive calls it makes.
// It corresponds to a logical thread in the deadlock detection literature.
type cycleChecker struct {
	waitsFor unsafe.Pointer // an *entry; see cycleCheck
}

func (cc *cycleChecker) setWaitsFor(e *entry) {
	atomic.StorePointer(&cc.waitsFor, unsafe.Pointer(e))
}

func (e *entry) setOwner(cc *cycleChecker) {
	atomic.StorePointer(&e.owner, unsafe.Pointer(cc))
}

// cycleCheck reports whether there is a path in the waits-for graph
// from resource 'e' to thread 'me'.
//
// The waits-for graph (WFG) is a bipartite graph whose nodes are
// alternately of type entry and cycleChecker.  Each node has at most
// one outgoing edge.  An entry has an "owner" edge to a cycleChecker
// while it is being readied by that cycleChecker, and a cycleChecker
// has a "waits-for" edge to an entry while it is waiting for that entry
// to become ready.
//
// Before adding a waits-for edge, the modCache checks whether the new edge
// would form a cycle.  If so, this indicates that the load graph is
// cyclic and that the following wait operation would deadlock.
func cycleCheck(e *entry, me *cycleChecker) error {
	for e != nil {
		cc := (*cycleChecker)(atomic.LoadPointer(&e.owner))
		if cc == nil {
			break
		}
		if cc == me {
			return fmt.Errorf("cycle in load graph")
		}
		e = (*entry)(atomic.LoadPointer(&cc.waitsFor))
	}
	return nil
}

func MakeLoad(src modOpener) func(thread *starlark.Thread, module string) (starlark.StringDict, error) {
	modCache := &modCache{
		modCache: make(map[string]*entry),
		src:      src,
	}

	return func(thread *starlark.Thread, module string) (starlark.StringDict, error) {
		return modCache.Load(thread, module)
	}
}

func convertJSONListToValue(iv []interface{}) (starlark.Value, error) {
	var res []starlark.Value
	for i, v := range iv {
		sv, err := convertJSONToValue(v)
		if err != nil {
			return nil, fmt.Errorf("invalid element %d, %w", i, err)
		}
		res = append(res, sv)
	}

	return starlark.NewList(res), nil
}

func convertJSONDictToValue(iv map[string]interface{}) (starlark.Value, error) {
	res := starlark.NewDict(len(iv))
	for k, v := range iv {
		sv, err := convertJSONToValue(v)
		if err != nil {
			return nil, fmt.Errorf("invalid element %s, %w", k, err)
		}
		res.SetKey(starlark.String(k), sv)
	}

	return res, nil
}

func goToJSONHack(iv interface{}) (interface{}, error) {
	bs, err := json.Marshal(iv)
	if err != nil {
		return nil, err
	}
	jres := make(map[string]interface{})
	err = json.Unmarshal(bs, &jres)
	if err != nil {
		return nil, err
	}
	return jres, nil
}

func convertJSONToValue(iv interface{}) (starlark.Value, error) {
	switch v := iv.(type) {
	case bool:
		return starlark.Bool(v), nil
	case float64:
		return starlark.Float(v), nil
	case string:
		return starlark.String(v), nil
	case []interface{}:
		return convertJSONListToValue(v)
	case map[string]interface{}:
		return convertJSONDictToValue(v)
	default:
		return nil, fmt.Errorf("could not convert %T to starlark type", iv)
	}
}

func convertToStarlarkValue(iv interface{}) (starlark.Value, error) {
	jv, err := goToJSONHack(iv)
	if err != nil {
		return nil, err
	}
	return convertJSONToValue(jv)
}

func convertValueToJSON(iv starlark.Value) (interface{}, error) {
	switch v := iv.(type) {
	case *starlark.Dict:
		return convertDictToJSON(v)
	case starlark.String:
		return string(v), nil
	case starlark.Bytes:
		return []byte(v), nil
	case starlark.Bool:
		return bool(v), nil
	case starlark.Int, starlark.Float:
		f, _ := starlark.AsFloat(iv)
		return f, nil
	case *starlark.List:
		return convertListToJSON(v)
	case starlark.NoneType, *starlark.Builtin:
		return nil, nil
	default:
		return nil, fmt.Errorf("could not convert %T to JSON type", iv)
	}
}

func convertListToJSON(l *starlark.List) ([]interface{}, error) {
	res := make([]interface{}, l.Len())
	for i := 0; i < l.Len(); i++ {
		var err error
		res[i], err = convertValueToJSON(l.Index(i))
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

func convertDictToJSON(v *starlark.Dict) (map[string]interface{}, error) {
	res := map[string]interface{}{}
	for _, knv := range v.Keys() {
		kv, ok, err := v.Get(knv)
		if !ok {
			panic("no idea what happened")
		}
		if err != nil {
			panic("no idea what happened")
		}
		jv, err := convertValueToJSON(kv)
		if err != nil {
			panic("no idea what happened")
		}

		switch k := knv.(type) {
		case starlark.String:
			res[string(k)] = jv
		default:
			continue
		}
	}
	return res, nil
}

func ConvertToWorkflow(v starlark.Value) (*workflow.Workflow, error) {
	wvDict, ok := v.(*starlark.Dict)
	if !ok {
		return nil, fmt.Errorf("workflow value is not a dictinoary")
	}

	js, err := convertDictToJSON(wvDict)
	if err != nil {
		return nil, fmt.Errorf("could not convert dictionary to json object, %w", err)
	}

	bs, err := json.Marshal(js)
	if err != nil {
		return nil, fmt.Errorf("could not marshal object to JSON, %w", err)
	}

	decode := scheme.Codecs.UniversalDeserializer().Decode
	obj, _, err := decode(bs, nil, nil)

	if err != nil {
		return nil, fmt.Errorf("could not decode JSON to kubernetes object, %w", err)
	}

	wf, ok := obj.(*workflow.Workflow)
	if !ok {
		return nil, fmt.Errorf("could not use %T as workflow", wf)
	}

	return wf, nil
}

type GithubEvent interface {
	GetInstallation() *github.Installation
	GetRepo() *github.Repository
	GetSender() *github.User
}

type WorkflowContext struct {
	Repo        *github.Repository
	Entrypoint  string
	Ref         string
	RefType     string
	SHA         string
	ContextData map[string]string
	PRs         []*github.PullRequest
	Event       GithubEvent
}

type PrintFunc func(_ *starlark.Thread, msg string)

type Config struct {
	// BuiltIns modules loaded via the "builtin:///"
	BuiltIns map[string]starlark.StringDict
	// Predeclared global variables
	Predeclared starlark.StringDict
	// Print function for user feedback
	Print PrintFunc
}

func DefaultBuiltIns() map[string]starlark.StringDict {
	yaml, _ := starlibyaml.LoadModule()
	re, _ := starlibre.LoadModule()

	// There should be a better way to inject these
	return map[string]starlark.StringDict{
		// The modules from qri have an annoying to use internal structure that
		// stutters the name
		"/encoding/yaml": yaml,
		"/re":            re,

		"/encoding/json": starlibjson.Module.Members,
		"/math":          starlibmath.Module.Members,
		"/time":          starlibtime.Module.Members,
	}
}

func buildInput(ciContext WorkflowContext) (starlark.Value, error) {
	inDict := starlark.StringDict{
		"ref":      starlark.String(ciContext.Ref),
		"ref_type": starlark.String(ciContext.RefType),
		"sha":      starlark.String(ciContext.SHA),
	}

	var slRepo starlark.Value
	slRepo, err := convertToStarlarkValue(ciContext.Repo)
	if err != nil {
		return nil, fmt.Errorf("could not convert repo to starlark value, %w", err)
	}
	inDict["repo"] = slRepo

	if ciContext.Event != nil {
		var slEv starlark.Value
		slEv, err = convertToStarlarkValue(ciContext.Event)
		if err != nil {
			return nil, fmt.Errorf("could not convert event to starlark, %w", err)
			// failed converting event
		}
		inDict["event"] = slEv
	}

	return starlarkstruct.FromStringDict(starlarkstruct.Default, inDict), nil
}

func LoadWorkflow(ctx context.Context, hc *http.Client, fn string, ciContext WorkflowContext, cfg Config) (*workflow.Workflow, error) {
	builtins := cfg.BuiltIns
	if builtins == nil {
		builtins = DefaultBuiltIns()
	}
	src := newModSource(hc)

	input, err := buildInput(ciContext)
	if err != nil {
		return nil, fmt.Errorf("could not build starlark input value, %w", err)
	}

	predeclared := cfg.Predeclared
	if predeclared == nil {
		predeclared = starlark.StringDict{}
	}

	/*
		"struct": starlark.NewBuiltin("struct", starlarkstruct.Make),
		"module": starlark.NewBuiltin("module", starlarkstruct.MakeModule),
	*/

	predeclared["loadFile"] = starlark.NewBuiltin("loadFile", src.builtInLoadFile)
	predeclared["input"] = input

	// This dictionary defines the pre-declared environment.
	src.SetPredeclared(predeclared)
	src.SetBuiltIn(builtins)
	src.SetContext(ciContext.ContextData)

	loader := MakeLoad(src)

	// The Thread defines the behavior of the built-in 'print' function.
	thread := &starlark.Thread{
		Name:  fn,
		Print: cfg.Print,
		Load:  loader,
	}

	u, _ := url.Parse(fmt.Sprintf("context:///%s", fn))
	setCWU(thread, u)

	script, ok := ciContext.ContextData[strings.TrimPrefix(fn, "/")]
	if !ok {
		return nil, fmt.Errorf("file %s is not present in the CI context", fn)
	}

	val, err := starlark.ExecFile(thread, fn, script, src.predeclared)
	if err != nil {
		// we explicitly do not wrap this error, we do not want ErrNotExist
		// to flow back to the caller as the config did exist but didn't
		// run. We allow the user to explicitly fall back by setting
		// workflow = None
		return nil, fmt.Errorf("starlark execution failed, %v", err)
	}

	if !val.Has("workflow") {
		return nil, fmt.Errorf("starlark result must contain 'workflow'")
	}
	wv := val["workflow"]

	if wv == starlark.None {
		return nil, fmt.Errorf("starlark workflow was None, treating as %w", os.ErrNotExist)
	}

	wf, err := ConvertToWorkflow(wv)
	if err != nil {
		return nil, fmt.Errorf("starlark result could not be marshalled to a workflow, %w", err)
	}

	return wf, nil
}

// Package fakehosting is an in-memory hosting.Engine (the contract in
// internal/hosting/engineapi) with scripted build
// phases. It is what the facade's unit tests run against, and what
// `HOSTING_API_URL=fake://` selects for local development without a
// Kubernetes cluster.
//
// Its behaviour is frozen by spec2 §4.1: nothing here is random and nothing
// depends on the wall clock. Builds advance only when a test calls Advance,
// timestamps come from Clock, and every id is a hash of its inputs — so a
// facade test that reruns is byte-for-byte the same run.
package fakehosting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/castlemilk/dns/internal/hosting/engineapi"
)

// MaxUploadBytes mirrors DeepHost's own cap; a larger archive is
// ResourceExhausted, exactly as the real Deploy stream reports it.
const MaxUploadBytes = 512 << 20

// Epoch is the instant the default clock starts at. It is a fixed, obviously
// synthetic time so a golden test never has to normalise it away.
var Epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Call is one engine call the fake recorded. Args never carry a secret: a git
// token is recorded as the boolean fact that one was supplied.
type Call struct {
	Method string
	Tenant string
	App    string
	Args   map[string]string
}

type app struct {
	tenant     string
	name       string
	framework  engineapi.Framework
	repository string
	branch     string
	rootDir    string
	// deletedAt is non-zero while a slow delete is draining.
	deletedAt time.Time
}

type build struct {
	id         string
	app        string
	phase      string
	reason     string
	revision   string
	repoURL    string
	failWith   string
	createdAt  time.Time
	startedAt  time.Time
	finishedAt time.Time
	seq        int
	lost       bool
	// digest is the archive hash for an upload build, so identical bytes find
	// the existing build instead of creating a second one.
	digest string
	// gitSHA is the commit the builder resolved the revision to. It is empty
	// for an upload build, exactly as DeepHost leaves it: an artifact upload
	// has no commit, and the engine reports nothing rather than guessing.
	gitSHA string
}

type release struct {
	id        string
	buildID   string
	app       string
	ready     bool
	weight    int32
	gitSHA    string
	createdAt time.Time
	seq       int
	// zeroSiblingsOn is the Advance counter at which a promotion's siblings
	// drop to weight 0. A real operator zeroes them one reconcile later, so a
	// poller that requires siblings at 0 observes one RELEASING tick.
	pendingPromotion bool
}

type domain struct {
	host       string
	app        string
	certOK     bool
	tlsMode    string
	redirectTo string
}

// envVar is one stored variable. The value is kept because a real engine keeps
// it — the point of the fake is that nothing but EnvValue, which no production
// code path can reach, ever reads it back.
type envVar struct {
	name        string
	environment string
	value       string
	lastSet     time.Time
}

// envKey scopes a variable to one app, environment and name, which is exactly
// DeepHost's `<environment>.<NAME>` data key inside the app's managed Secret.
func envKey(appName, environment, name string) string {
	return appName + "\x00" + environment + "\x00" + name
}

type logs struct {
	lines     []string
	complete  bool
	truncated bool
}

// Engine is the in-memory hosting engine.
type Engine struct {
	mu sync.Mutex

	// Clock is the fake's time source. It defaults to a fixed instant advanced
	// by one second per Advance.
	clock func() time.Time
	now   time.Time

	apps     map[string]*app
	builds   map[string]*build
	releases map[string]*release
	domains  map[string]*domain
	logsByID map[string]logs
	envVars  map[string]*envVar

	foreignApps  map[string]struct{}
	foreignHosts map[string]string

	oldServer  bool
	health     error
	deployErr  error
	slowDelete time.Duration
	failNext   string
	tlsMode    string

	counter int
	seq     int
	calls   []Call
	// promoteZeroPending is true when a PromoteRelease is waiting for the next
	// Advance to zero the siblings.
	promoteZeroPending bool
}

// New builds an empty fake engine.
func New() *Engine {
	engine := &Engine{
		apps:         map[string]*app{},
		builds:       map[string]*build{},
		releases:     map[string]*release{},
		domains:      map[string]*domain{},
		logsByID:     map[string]logs{},
		envVars:      map[string]*envVar{},
		foreignApps:  map[string]struct{}{},
		foreignHosts: map[string]string{},
		tlsMode:      engineapi.TLSModeIssued,
		now:          Epoch,
	}
	engine.clock = func() time.Time { return engine.now }
	return engine
}

// at reads the fake's time source. The default clock returns the instant
// Advance last moved to; an installed clock (a test's) wins, so the fake and
// the facade under test agree on what "now" is.
func (e *Engine) at() time.Time { return e.clock() }

// Clock returns the fake's time source, so a facade under test shares it.
func (e *Engine) Clock() func() time.Time {
	return func() time.Time {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.at()
	}
}

// SetClock replaces the time source. The default advances one second per
// Advance; a test that needs its own schedule installs one here.
func (e *Engine) SetClock(clock func() time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if clock != nil {
		e.clock = clock
	}
}

// SetOldServer switches to a server that predates ListBuilds, DeleteDomain,
// the environment-variable RPCs, Domain.redirect_to, Release.git_sha, the build
// timestamps and the domain readiness fields.
func (e *Engine) SetOldServer(old bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.oldServer = old
}

// SetHealth makes Health return err (nil restores health).
func (e *Engine) SetHealth(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.health = err
}

// SetDeployError fails the next Deploy stream with err.
func (e *Engine) SetDeployError(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deployErr = err
}

// SetSlowDelete keeps an app's hosts listed for d after DeleteApp, so a test
// can exercise the detach path that waits for the engine's garbage collection.
func (e *Engine) SetSlowDelete(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.slowDelete = d
}

// FailNextBuild makes the next build created end Failed with reason.
func (e *Engine) FailNextBuild(reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failNext = reason
}

// LoseBuild makes GetBuild report NotFound for id from now on.
func (e *Engine) LoseBuild(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if value, ok := e.builds[id]; ok {
		value.lost = true
	}
}

// SetLogs fixes the log lines a build returns.
func (e *Engine) SetLogs(id string, lines []string, complete bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logsByID[id] = logs{lines: append([]string(nil), lines...), complete: complete}
}

// SetLogsTruncated makes a build return 256 KiB of lines with complete=false.
func (e *Engine) SetLogsTruncated(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	line := strings.Repeat("x", 255)
	lines := make([]string, 0, 1100)
	for len(lines)*256 < 256<<10 {
		lines = append(lines, line)
	}
	e.logsByID[id] = logs{lines: lines, complete: false, truncated: true}
}

// SetCertificateReady flips one host's certificate to ready.
func (e *Engine) SetCertificateReady(host string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if value, ok := e.domains[host]; ok {
		value.certOK = true
	}
}

// SetTLSMode switches every host onto mode ("issued", "shared" or "none").
func (e *Engine) SetTLSMode(mode string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tlsMode = mode
	for _, value := range e.domains {
		value.tlsMode = mode
	}
}

// SeedForeignApp marks an app name as owned by another tenant.
func (e *Engine) SeedForeignApp(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.foreignApps[name] = struct{}{}
}

// SeedForeignHost marks a host as already served by another app.
func (e *Engine) SeedForeignHost(host, app string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.foreignHosts[strings.ToLower(host)] = app
}

// ReleaseForeignHost frees a host another app was holding, the way detaching
// that site does. It is what makes a "this host is taken" refusal recoverable
// in a test rather than permanent.
func (e *Engine) ReleaseForeignHost(host string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.foreignHosts, strings.ToLower(strings.TrimSpace(host)))
}

// Calls returns every recorded call in order.
func (e *Engine) Calls() []Call {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Call(nil), e.calls...)
}

// Advance moves the clock one second and every non-terminal build one step. It
// returns the phase of the build that moved last, which is what a table test
// asserts against.
func (e *Engine) Advance() string {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.now = e.now.Add(time.Second)

	if e.promoteZeroPending {
		e.zeroSiblingsLocked()
		e.promoteZeroPending = false
	}

	ids := make([]string, 0, len(e.builds))
	for id := range e.builds {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(left, right string) int { return e.builds[left].seq - e.builds[right].seq })

	last := ""
	for _, id := range ids {
		value := e.builds[id]
		next, moved := nextPhase(value)
		if !moved {
			continue
		}
		value.phase = next
		switch next {
		case engineapi.BuildPhaseBuilding:
			value.startedAt = e.at()
		case engineapi.BuildPhaseSucceeded:
			value.finishedAt = e.at()
			e.createReleaseLocked(value)
		case engineapi.BuildPhaseFailed:
			value.finishedAt = e.at()
			value.reason = value.failWith
		}
		last = next
	}
	return last
}

func nextPhase(value *build) (string, bool) {
	switch value.phase {
	case "":
		return engineapi.BuildPhasePending, true
	case engineapi.BuildPhasePending:
		if value.failWith != "" {
			return engineapi.BuildPhaseFailed, true
		}
		return engineapi.BuildPhaseBuilding, true
	case engineapi.BuildPhaseBuilding:
		if value.failWith != "" {
			return engineapi.BuildPhaseFailed, true
		}
		return engineapi.BuildPhaseSucceeded, true
	default:
		return value.phase, false
	}
}

func (e *Engine) createReleaseLocked(value *build) {
	id := "rel-" + value.id
	if _, ok := e.releases[id]; ok {
		return
	}
	e.seq++
	for _, sibling := range e.releases {
		if sibling.app == value.app {
			sibling.weight = 0
		}
	}
	e.releases[id] = &release{
		id:        id,
		buildID:   value.id,
		app:       value.app,
		ready:     true,
		weight:    100,
		gitSHA:    value.gitSHA,
		createdAt: e.at(),
		seq:       e.seq,
	}
}

func (e *Engine) zeroSiblingsLocked() {
	for _, promoted := range e.releases {
		if !promoted.pendingPromotion {
			continue
		}
		promoted.pendingPromotion = false
		for _, sibling := range e.releases {
			if sibling.app == promoted.app && sibling.id != promoted.id {
				sibling.weight = 0
			}
		}
	}
}

func (e *Engine) record(method, tenant, app string, args map[string]string) {
	e.calls = append(e.calls, Call{Method: method, Tenant: tenant, App: app, Args: args})
}

// slug mirrors DeepHost's own slug: lowercase, runs of non `[a-z0-9-]`
// collapsed to one dash, dashes trimmed from both ends.
func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	dash := false
	for _, char := range value {
		switch {
		case (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9'):
			out.WriteRune(char)
			dash = false
		default:
			if !dash && out.Len() > 0 {
				out.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(out.String(), "-")
}

func appKey(tenant, name string) string { return slug(tenant) + "-" + slug(name) }

// Health reports the scripted health.
func (e *Engine) Health(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.record("Health", "", "", nil)
	return e.health
}

func (e *Engine) CreateApp(ctx context.Context, tenant, name string, spec engineapi.AppSpec) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	key := appKey(tenant, name)
	e.record("CreateApp", tenant, name, map[string]string{"framework": string(spec.Framework)})
	if _, taken := e.foreignApps[key]; taken {
		return "", connect.NewError(connect.CodeAlreadyExists,
			fmt.Errorf("app name %q belongs to another tenant or app", key))
	}
	if _, exists := e.apps[key]; exists {
		return "", connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("app %q already exists", key))
	}
	e.apps[key] = &app{
		tenant:     slug(tenant),
		name:       key,
		framework:  spec.Framework,
		repository: spec.Repository,
		branch:     spec.Branch,
		rootDir:    spec.RootDirectory,
	}
	return key, nil
}

func (e *Engine) UpdateApp(ctx context.Context, tenant, name string, patch engineapi.AppPatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.record("UpdateApp", tenant, name, nil)
	value, err := e.appLocked(tenant, name)
	if err != nil {
		return err
	}
	if patch.Repository != nil {
		value.repository = *patch.Repository
	}
	if patch.Branch != nil {
		value.branch = *patch.Branch
	}
	if patch.RootDirectory != nil {
		value.rootDir = *patch.RootDirectory
	}
	return nil
}

func (e *Engine) GetApp(ctx context.Context, tenant, name string) (engineapi.AppView, error) {
	if err := ctx.Err(); err != nil {
		return engineapi.AppView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.record("GetApp", tenant, name, nil)
	value, err := e.appLocked(tenant, name)
	if err != nil {
		return engineapi.AppView{}, err
	}
	return e.viewLocked(value), nil
}

func (e *Engine) ListApps(ctx context.Context, tenant string) ([]engineapi.AppView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.record("ListApps", tenant, "", nil)
	names := make([]string, 0, len(e.apps))
	for name, value := range e.apps {
		if value.tenant != slug(tenant) {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	views := make([]engineapi.AppView, 0, len(names))
	for _, name := range names {
		views = append(views, e.viewLocked(e.apps[name]))
	}
	return views, nil
}

func (e *Engine) DeleteApp(ctx context.Context, tenant, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.record("DeleteApp", tenant, name, nil)
	key := appKey(tenant, name)
	value, ok := e.apps[key]
	if !ok || value.tenant != slug(tenant) {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("app %q not found", key))
	}
	delete(e.apps, key)
	for id, item := range e.builds {
		if item.app == key {
			delete(e.builds, id)
			delete(e.logsByID, id)
		}
	}
	for id, item := range e.releases {
		if item.app == key {
			delete(e.releases, id)
		}
	}
	if e.slowDelete <= 0 {
		e.removeHostsLocked(key)
		return nil
	}
	value.deletedAt = e.at().Add(e.slowDelete)
	// Keep the app row only as the marker the slow garbage collection reads.
	e.apps[key] = value
	return nil
}

func (e *Engine) removeHostsLocked(appName string) {
	for host, value := range e.domains {
		if value.app == appName {
			delete(e.domains, host)
		}
	}
}

func (e *Engine) appLocked(tenant, name string) (*app, error) {
	key := appKey(tenant, name)
	value, ok := e.apps[key]
	if !ok || value.tenant != slug(tenant) || !value.deletedAt.IsZero() {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("app %q not found", key))
	}
	return value, nil
}

func (e *Engine) viewLocked(value *app) engineapi.AppView {
	hosts := make([]string, 0, len(e.domains))
	for host, item := range e.domains {
		// A redirecting host is deliberately absent from App.spec.domains: it
		// is not in the router's hostname map, only in the gateway's redirect
		// route. DeepHost projects the same list, so the facade meets the same
		// gap here that it does on a cluster.
		if item.app == value.name && item.redirectTo == "" {
			hosts = append(hosts, host)
		}
	}
	slices.Sort(hosts)

	latest := ""
	best := 0
	for _, item := range e.releases {
		if item.app == value.name && item.weight == 100 && item.seq > best {
			latest, best = item.id, item.seq
		}
	}
	return engineapi.AppView{
		Name:          value.name,
		Framework:     value.framework,
		Domains:       hosts,
		LatestRelease: latest,
		Ready:         latest != "",
		Repository:    value.repository,
		Branch:        value.branch,
	}
}

func (e *Engine) CreateGitBuild(ctx context.Context, tenant, name string, in engineapi.GitBuildInput) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.record("CreateGitBuild", tenant, name, map[string]string{
		"repo_url":  in.RepoURL,
		"revision":  in.Revision,
		"path":      in.Path,
		"git_token": fmt.Sprintf("%t", in.GitToken != ""),
	})
	value, err := e.appLocked(tenant, name)
	if err != nil {
		return "", err
	}

	e.counter++
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s\x00%s\x00%d",
		slug(tenant), value.name, in.RepoURL, in.Revision, e.counter))
	id := "b-" + hex.EncodeToString(sum[:])[:12]
	e.newBuildLocked(id, value.name, in.Revision, in.RepoURL, "")
	return id, nil
}

func (e *Engine) newBuildLocked(id, appName, revision, repoURL, digest string) *build {
	e.seq++
	value := &build{
		id:        id,
		app:       appName,
		revision:  revision,
		repoURL:   repoURL,
		createdAt: e.at(),
		seq:       e.seq,
		digest:    digest,
	}
	if repoURL != "" {
		// A git build resolves to a commit. The fake derives a stable
		// 40-character lowercase hex id from the build id so a test can assert
		// on the exact value without hard-coding a magic constant.
		sum := sha256.Sum256([]byte("commit\x00" + id))
		value.gitSHA = hex.EncodeToString(sum[:])[:40]
	}
	if e.failNext != "" {
		value.failWith = e.failNext
		e.failNext = ""
	}
	e.builds[id] = value
	return value
}

func (e *Engine) Deploy(
	ctx context.Context,
	tenant, name string,
	framework engineapi.Framework,
	archive io.Reader,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(archive, MaxUploadBytes+1))
	if err != nil {
		return "", err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.record("Deploy", tenant, name, map[string]string{
		"framework": string(framework),
		"bytes":     fmt.Sprintf("%d", written),
	})
	if e.deployErr != nil {
		failure := e.deployErr
		e.deployErr = nil
		return "", failure
	}
	if written > MaxUploadBytes {
		return "", connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("deployment exceeds %d bytes", MaxUploadBytes))
	}

	appName := appKey(tenant, name)
	if _, ok := e.apps[appName]; !ok {
		// DeepHost's Deploy auto-creates the app; the fake does the same so a
		// facade path that relies on it is exercised honestly.
		e.apps[appName] = &app{tenant: slug(tenant), name: appName, framework: framework}
	}

	digest := hex.EncodeToString(hash.Sum(nil))
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s", slug(tenant), appName, digest))
	id := "b-" + hex.EncodeToString(sum[:])[:12]
	if _, exists := e.builds[id]; exists {
		return id, nil
	}
	e.newBuildLocked(id, appName, "", "", digest)
	return id, nil
}

func (e *Engine) GetBuild(ctx context.Context, tenant, name, buildID string) (engineapi.BuildView, error) {
	if err := ctx.Err(); err != nil {
		return engineapi.BuildView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.record("GetBuild", tenant, name, map[string]string{"build_id": buildID})
	value, ok := e.builds[buildID]
	if !ok || value.lost || value.app != appKey(tenant, name) {
		return engineapi.BuildView{}, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("build %q not found", buildID))
	}
	return e.buildViewLocked(value), nil
}

func (e *Engine) buildViewLocked(value *build) engineapi.BuildView {
	view := engineapi.BuildView{
		ID:       value.id,
		Phase:    value.phase,
		Reason:   value.reason,
		Revision: value.revision,
		RepoURL:  value.repoURL,
	}
	if value.phase == engineapi.BuildPhaseSucceeded {
		view.ReleaseID = "rel-" + value.id
	}
	if !e.oldServer {
		view.CreatedAt = value.createdAt
		view.StartedAt = value.startedAt
		view.FinishedAt = value.finishedAt
	}
	return view
}

func (e *Engine) ListBuilds(ctx context.Context, tenant, name string, limit int) ([]engineapi.BuildView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.record("ListBuilds", tenant, name, nil)
	if e.oldServer {
		return nil, engineapi.ErrUnsupported
	}
	appName := appKey(tenant, name)
	matched := make([]*build, 0, len(e.builds))
	for _, value := range e.builds {
		if value.app == appName && !value.lost {
			matched = append(matched, value)
		}
	}
	slices.SortFunc(matched, func(left, right *build) int { return right.seq - left.seq })
	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}
	views := make([]engineapi.BuildView, 0, len(matched))
	for _, value := range matched {
		views = append(views, e.buildViewLocked(value))
	}
	return views, nil
}

func (e *Engine) GetBuildLogs(ctx context.Context, tenant, name, buildID string, tail int) ([]string, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.record("GetBuildLogs", tenant, name, map[string]string{"build_id": buildID})
	value, ok := e.builds[buildID]
	if !ok || value.lost || value.app != appKey(tenant, name) {
		return nil, false, connect.NewError(connect.CodeNotFound, fmt.Errorf("build %q not found", buildID))
	}
	if scripted, ok := e.logsByID[buildID]; ok {
		lines := scripted.lines
		if tail > 0 && len(lines) > tail {
			lines = lines[len(lines)-tail:]
		}
		return append([]string(nil), lines...), scripted.complete, nil
	}
	lines := []string{fmt.Sprintf("fake: build %s %s", value.id, phaseLabel(value.phase))}
	return lines, value.phase == engineapi.BuildPhaseSucceeded || value.phase == engineapi.BuildPhaseFailed, nil
}

func phaseLabel(phase string) string {
	if phase == "" {
		return "created"
	}
	return phase
}

func (e *Engine) ListReleases(ctx context.Context, tenant, name string) ([]engineapi.ReleaseView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.record("ListReleases", tenant, name, nil)
	appName := appKey(tenant, name)
	matched := make([]*release, 0, len(e.releases))
	for _, value := range e.releases {
		if value.app == appName {
			matched = append(matched, value)
		}
	}
	slices.SortFunc(matched, func(left, right *release) int { return right.seq - left.seq })
	views := make([]engineapi.ReleaseView, 0, len(matched))
	for _, value := range matched {
		created := value.createdAt
		if e.oldServer {
			created = time.Time{}
		}
		sha := value.gitSHA
		if e.oldServer {
			// A server that predates the resolved commit reports none, which
			// is what the facade must degrade to.
			sha = ""
		}
		views = append(views, engineapi.ReleaseView{
			ID:            value.id,
			BuildID:       value.buildID,
			Ready:         value.ready,
			TrafficWeight: value.weight,
			GitSHA:        sha,
			CreatedAt:     created,
		})
	}
	return views, nil
}

func (e *Engine) PromoteRelease(ctx context.Context, tenant, name, releaseID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.record("PromoteRelease", tenant, name, map[string]string{"release_id": releaseID})
	value, ok := e.releases[releaseID]
	if !ok || value.app != appKey(tenant, name) {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("release %q not found", releaseID))
	}
	value.weight = 100
	value.ready = true
	value.pendingPromotion = true
	e.promoteZeroPending = true
	return nil
}

// CreateDomain registers a host and, on a second call for a host this app
// already owns, updates its redirect mode — DeepHost has no UpdateDomain, so
// this is the only way the mode changes.
func (e *Engine) CreateDomain(ctx context.Context, tenant, name string, spec engineapi.DomainSpec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	host := strings.ToLower(strings.TrimSpace(spec.Host))
	redirectTo := strings.ToLower(strings.TrimSpace(spec.RedirectTo))
	e.record("CreateDomain", tenant, name, map[string]string{
		"host": host, "tls_secret": spec.TLSSecret, "redirect_to": redirectTo,
	})
	value, err := e.appLocked(tenant, name)
	if err != nil {
		return err
	}
	if redirectTo == host {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("redirect_to must differ from host %q", host))
	}
	if owner, taken := e.foreignHosts[host]; taken && owner != value.name {
		return connect.NewError(connect.CodeAlreadyExists,
			fmt.Errorf("domain %q belongs to app %q", host, owner))
	}
	if existing, ok := e.domains[host]; ok {
		if existing.app != value.name {
			return connect.NewError(connect.CodeAlreadyExists,
				fmt.Errorf("domain %q belongs to app %q", host, existing.app))
		}
		if !e.oldServer {
			existing.redirectTo = redirectTo
		}
		return nil
	}
	mode := e.tlsMode
	if spec.TLSSecret != "" {
		mode = engineapi.TLSModeShared
	}
	registered := &domain{host: host, app: value.name, tlsMode: mode}
	if !e.oldServer {
		// An older server has no redirectTo field at all: it accepts the call
		// and silently serves the app on the host.
		registered.redirectTo = redirectTo
	}
	e.domains[host] = registered
	return nil
}

func (e *Engine) ListDomains(ctx context.Context, tenant, name string) ([]engineapi.DomainView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.record("ListDomains", tenant, name, nil)
	e.expireSlowDeletesLocked()

	appName := appKey(tenant, name)
	if _, ok := e.apps[appName]; !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("app %q not found", appName))
	}
	hosts := make([]string, 0, len(e.domains))
	for host, value := range e.domains {
		if value.app == appName {
			hosts = append(hosts, host)
		}
	}
	slices.Sort(hosts)
	views := make([]engineapi.DomainView, 0, len(hosts))
	for _, host := range hosts {
		value := e.domains[host]
		view := engineapi.DomainView{Host: host, App: appName}
		if !e.oldServer {
			view.RedirectTo = value.redirectTo
			view.ReadinessReported = true
			view.Ready = true
			view.TLSMode = value.tlsMode
			view.CertificateReady = value.certOK && value.tlsMode == engineapi.TLSModeIssued
			if !view.CertificateReady && value.tlsMode == engineapi.TLSModeIssued {
				view.Reason = "certificate not created yet"
			}
		}
		views = append(views, view)
	}
	return views, nil
}

// expireSlowDeletesLocked completes a slow DeleteApp once the clock passed its
// deadline, so a detach that polls ListDomains eventually sees an empty list.
func (e *Engine) expireSlowDeletesLocked() {
	for key, value := range e.apps {
		if value.deletedAt.IsZero() || e.at().Before(value.deletedAt) {
			continue
		}
		delete(e.apps, key)
		e.removeHostsLocked(key)
	}
}

func (e *Engine) DeleteDomain(ctx context.Context, tenant, name, host string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	host = strings.ToLower(strings.TrimSpace(host))
	e.record("DeleteDomain", tenant, name, map[string]string{"host": host})
	if e.oldServer {
		return engineapi.ErrUnsupported
	}
	value, ok := e.domains[host]
	if !ok {
		return nil
	}
	if value.app != appKey(tenant, name) {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("domain %q not found", host))
	}
	delete(e.domains, host)
	return nil
}

// Environment-variable bounds, copied from DeepHost's control plane so the
// facade meets the same refusals here as it does on a cluster.
const (
	// MaxEnvValueBytes is the largest value the engine accepts.
	MaxEnvValueBytes = 32 << 10
	// MaxEnvVars is how many variables one app may hold, across environments.
	MaxEnvVars = 100
)

// SetAppEnvVar stores one variable. The value is kept — that is what an engine
// is for — but no method on this type returns it except EnvValue, which exists
// solely so a test can prove the value arrived intact.
func (e *Engine) SetAppEnvVar(
	ctx context.Context,
	tenant, name string,
	in engineapi.EnvVarInput,
) (engineapi.EnvVarView, error) {
	if err := ctx.Err(); err != nil {
		return engineapi.EnvVarView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	environment := envOrProduction(in.Environment)
	// The recorded call names the variable and the environment. The value is
	// recorded as the boolean fact that one was supplied, exactly as
	// CreateGitBuild records the git token.
	e.record("SetAppEnvVar", tenant, name, map[string]string{
		"name":        in.Name,
		"environment": environment,
		"value":       fmt.Sprintf("%t", in.Value != ""),
	})
	if e.oldServer {
		return engineapi.EnvVarView{}, engineapi.ErrUnsupported
	}
	value, err := e.appLocked(tenant, name)
	if err != nil {
		return engineapi.EnvVarView{}, err
	}
	if in.Name == "" {
		return engineapi.EnvVarView{}, connect.NewError(connect.CodeInvalidArgument,
			errors.New("name is required"))
	}
	if len(in.Value) > MaxEnvValueBytes {
		return engineapi.EnvVarView{}, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("value exceeds %d bytes", MaxEnvValueBytes))
	}
	key := envKey(value.name, environment, in.Name)
	if _, exists := e.envVars[key]; !exists && e.countEnvVarsLocked(value.name) >= MaxEnvVars {
		return engineapi.EnvVarView{}, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("app %q already holds %d variables", value.name, MaxEnvVars))
	}
	stored := &envVar{name: in.Name, environment: environment, value: in.Value, lastSet: e.at()}
	e.envVars[key] = stored
	return engineapi.EnvVarView{Name: stored.name, Environment: stored.environment, LastSet: stored.lastSet}, nil
}

func (e *Engine) DeleteAppEnvVar(ctx context.Context, tenant, name, environment, variable string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	environment = envOrProduction(environment)
	e.record("DeleteAppEnvVar", tenant, name, map[string]string{
		"name": variable, "environment": environment,
	})
	if e.oldServer {
		return engineapi.ErrUnsupported
	}
	value, err := e.appLocked(tenant, name)
	if err != nil {
		return err
	}
	key := envKey(value.name, environment, variable)
	if _, ok := e.envVars[key]; !ok {
		return connect.NewError(connect.CodeNotFound,
			fmt.Errorf("variable %q not found in %s", variable, environment))
	}
	delete(e.envVars, key)
	return nil
}

// ListAppEnvVars returns names, environments and stamps, sorted by environment
// then name — the order DeepHost returns them in.
func (e *Engine) ListAppEnvVars(
	ctx context.Context,
	tenant, name, environment string,
) ([]engineapi.EnvVarView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.record("ListAppEnvVars", tenant, name, map[string]string{"environment": environment})
	if e.oldServer {
		return nil, engineapi.ErrUnsupported
	}
	value, err := e.appLocked(tenant, name)
	if err != nil {
		return nil, err
	}
	prefix := value.name + "\x00"
	views := make([]engineapi.EnvVarView, 0, len(e.envVars))
	for key, stored := range e.envVars {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if environment != "" && stored.environment != environment {
			continue
		}
		views = append(views, engineapi.EnvVarView{
			Name: stored.name, Environment: stored.environment, LastSet: stored.lastSet,
		})
	}
	slices.SortFunc(views, func(left, right engineapi.EnvVarView) int {
		if left.Environment != right.Environment {
			return strings.Compare(left.Environment, right.Environment)
		}
		return strings.Compare(left.Name, right.Name)
	})
	return views, nil
}

func (e *Engine) countEnvVarsLocked(appName string) int {
	prefix := appName + "\x00"
	count := 0
	for key := range e.envVars {
		if strings.HasPrefix(key, prefix) {
			count++
		}
	}
	return count
}

// EnvValue reads back a stored value. It is a test accessor with no counterpart
// on engineapi.Engine, so no facade code path can reach it: a test that asserts
// the value arrived intact proves the write-only contract rather than weakening
// it.
func (e *Engine) EnvValue(tenant, name, environment, variable string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stored, ok := e.envVars[envKey(appKey(tenant, name), envOrProduction(environment), variable)]
	if !ok {
		return "", false
	}
	return stored.value, true
}

func envOrProduction(environment string) string {
	if environment == "" {
		return engineapi.EnvironmentProduction
	}
	return environment
}

// ErrScripted is a convenience for tests that only need "the engine failed".
var ErrScripted = errors.New("fakehosting: scripted failure")

var _ engineapi.Engine = (*Engine)(nil)

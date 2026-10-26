package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"maps"
	"net/http"
	"sort"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimequota "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	domain "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

type affinityRuntime struct {
	settings     internalconfig.QuotaAffinitySettings
	store        *affinity.FileStore
	retention    affinity.Retention
	busy         map[affinity.ChatKey]bool
	observations map[string]quotaRecord
	now          func() time.Time
	pollSlots    chan struct{}
	polling      map[string]bool
	history      *quotaHistory
}

type quotaRecord struct {
	ref     affinity.CredentialRef
	windows map[string]domain.Snapshot
}
type affinityPinKey struct{}
type affinityPin struct{ ref affinity.CredentialRef }

// ConfigureQuotaAffinity opens the manager-owned store once
// A running profile cannot change or be removed by reload; restart is required
func (m *Manager) ConfigureQuotaAffinity(cfg *internalconfig.Config) error {
	settings, err := cfg.QuotaAffinitySettings()
	if err != nil {
		return err
	}
	m.affinityMu.Lock()
	defer m.affinityMu.Unlock()
	if m.affinityRuntime != nil {
		if settings != m.affinityRuntime.settings {
			return errors.New("quota affinity reload requires an unchanged profile; restart required")
		}
		return nil
	}
	if settings.Namespace == "" {
		return nil
	}
	retention, err := affinity.NewRetention(settings.IdleRetention, settings.HardRetention, settings.MaxBindings)
	if err != nil {
		return err
	}
	runtime := &affinityRuntime{settings: settings, retention: retention, busy: make(map[affinity.ChatKey]bool), observations: make(map[string]quotaRecord), now: time.Now, pollSlots: make(chan struct{}, settings.PollConcurrency), polling: make(map[string]bool)}
	store, err := affinity.OpenFileStoreWithClock(settings.StateDir, retention, func() time.Time { return runtime.now() })
	if err != nil {
		return err
	}
	runtime.store = store
	// history persistence is best effort and never fails profile startup
	runtime.history = openQuotaHistory(settings.StateDir, func() time.Time { return runtime.now() })
	m.affinityRuntime = runtime
	return nil
}

// ValidateQuotaAffinityReload rejects profile changes before any runtime config commit
func (m *Manager) ValidateQuotaAffinityReload(cfg *internalconfig.Config) error {
	settings, err := cfg.QuotaAffinitySettings()
	if err != nil {
		return err
	}
	m.affinityMu.Lock()
	defer m.affinityMu.Unlock()
	if m.affinityRuntime == nil {
		if settings.Namespace != "" {
			return errors.New("quota affinity enable requires restart")
		}
		return nil
	}
	if settings != m.affinityRuntime.settings {
		return errors.New("quota affinity reload requires an unchanged profile; restart required")
	}
	return nil
}

// CloseQuotaAffinity closes the local durable store after request cancellation
func (m *Manager) CloseQuotaAffinity() error {
	m.affinityMu.Lock()
	defer m.affinityMu.Unlock()
	if m.affinityRuntime == nil {
		return nil
	}
	// drain history writes before the store releases the directory lock
	m.affinityRuntime.history.close()
	return m.affinityRuntime.store.Close()
}

// QuotaAffinityNamespace returns the enabled deployment namespace, or empty
func (m *Manager) QuotaAffinityNamespace() string {
	if m == nil {
		return ""
	}
	m.affinityMu.Lock()
	defer m.affinityMu.Unlock()
	if m.affinityRuntime == nil {
		return ""
	}
	return m.affinityRuntime.settings.Namespace
}

// QuotaAffinityAllowsLegacy reports whether requests without chat headers may use fallback routing
func (m *Manager) QuotaAffinityAllowsLegacy() bool {
	if m == nil {
		return false
	}
	m.affinityMu.Lock()
	defer m.affinityMu.Unlock()
	return m.affinityRuntime != nil && m.affinityRuntime.settings.AllowLegacyRequests
}

// NewChatRequest creates an SDK request from an authenticated host caller
// The host must not obtain caller identity from untrusted client identity headers
func (m *Manager) NewChatRequest(caller affinity.CallerScope, chat affinity.ChatID, intent affinity.Intent, handle affinity.Handle) (affinity.Request, error) {
	key, err := affinity.NewChatKey(m.QuotaAffinityNamespace(), caller, chat)
	if err != nil {
		return affinity.Request{}, affinity.NewError(affinity.IdentityInvalid, time.Time{})
	}
	switch intent {
	case affinity.StartIntent:
		return affinity.Start(key), nil
	case affinity.ContinueIntent:
		r := affinity.Continue(key, handle)
		return r, r.Validate()
	default:
		return affinity.Request{}, affinity.NewError(affinity.IdentityInvalid, time.Time{})
	}
}

func (m *Manager) credentialRef(a *Auth) (affinity.CredentialRef, error) {
	if a == nil {
		return affinity.CredentialRef{}, affinity.NewError(affinity.CredentialMissing, time.Time{})
	}
	if executorKeyFromAuth(a) != a.Provider || (a.Provider != "claude" && a.Provider != "codex") {
		return affinity.CredentialRef{}, affinity.NewError(affinity.RouteUnsupported, time.Time{})
	}
	var lineage [32]byte
	if a.AuthKind() == AuthKindAPIKey {
		key, origin := a.Attributes["api_key"], credentialOrigin(a)
		if key == "" {
			return affinity.CredentialRef{}, affinity.NewError(affinity.CredentialChanged, time.Time{})
		}

		m.affinityMu.Lock()
		r := m.affinityRuntime
		m.affinityMu.Unlock()
		if r == nil {
			return affinity.CredentialRef{}, affinity.NewError(affinity.RouteUnsupported, time.Time{})
		}
		var err error
		lineage, err = r.store.APIKeyLineage(context.Background(), key, origin)
		if err != nil {
			return affinity.CredentialRef{}, err
		}
	} else {
		provenance, errProvenance := affinity.ParseOAuthProvenance(a.Metadata["oauth_provenance"])
		identityKey := "account_uuid"
		if a.Provider == "codex" {
			identityKey = "account_id"
		}
		identity, _ := a.Metadata[identityKey].(string)
		token, _ := a.Metadata["access_token"].(string)
		if identity == "" || errProvenance != nil || !provenance.Matches(a.Provider, identity, token) {
			return affinity.CredentialRef{}, affinity.NewError(affinity.CredentialChanged, time.Time{})
		}
		organization, _ := a.Metadata["organization_uuid"].(string)
		lineage = sha256.Sum256([]byte("cliproxy/oauth-principal/v1\x00" + a.Provider + "\x00" + identity + "\x00" + organization + "\x00" + credentialOrigin(a)))
	}
	ref, err := affinity.NewCredentialRef(a.Provider, a.ID, lineage)
	if err != nil {
		return ref, affinity.NewError(affinity.CredentialChanged, time.Time{})
	}
	return ref, nil
}

func (m *Manager) affinityView(ref affinity.CredentialRef, model string) domain.View {
	m.affinityMu.Lock()
	defer m.affinityMu.Unlock()
	r := m.affinityRuntime
	if r == nil {
		return domain.View{}
	}
	record := r.observations[ref.AuthID()]
	if record.ref != ref {
		return domain.View{}
	}
	observations := make([]domain.Snapshot, 0, len(record.windows))
	for _, s := range record.windows {
		observations = append(observations, s)
	}
	return domain.Combine(observations, model, r.now(), r.settings.MaxQuotaAge)
}

func (m *Manager) validateAffinityAuth(ctx context.Context, ref affinity.CredentialRef, model string, opts exec.Options) (*Auth, ProviderExecutor, error) {
	if m.HomeEnabled() || m.hasPluginScheduler() {
		return nil, nil, affinity.NewError(affinity.RouteUnsupported, time.Time{})
	}
	a, ok := m.GetByID(ref.AuthID())
	if !ok {
		return nil, nil, affinity.NewError(affinity.CredentialMissing, time.Time{})
	}
	current, err := m.credentialRef(a)
	if err != nil || current != ref {
		return nil, nil, affinity.NewError(affinity.CredentialChanged, time.Time{})
	}
	if a.Disabled || a.Status == StatusDisabled {
		return nil, nil, affinity.NewError(affinity.CredentialDisabled, time.Time{})
	}
	if !authSelectionEligibilityForRequest(ctx, opts).allows(a) || authWeight(a) <= 0 {
		return nil, nil, affinity.NewError(affinity.CredentialDisabled, time.Time{})
	}
	if !m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, model) {
		return nil, nil, affinity.NewError(affinity.ModelUnsupported, time.Time{})
	}
	blocked, _, next := isAuthBlockedForModel(a, m.selectionModelForAuth(a, model), m.affinityNow())
	if blocked {
		return nil, nil, affinity.NewError(affinity.CredentialCooldown, next)
	}
	if m.affinityView(ref, model).Denied() {
		return nil, nil, affinity.NewError(affinity.QuotaDenied, time.Time{})
	}
	e, ok := m.Executor(ref.Provider())
	if !ok {
		return nil, nil, affinity.NewError(affinity.RouteUnsupported, time.Time{})
	}
	authority, okAuthority := e.(exec.LocalExecutionAuthority)
	if !okAuthority || authority.LocalBuiltinProvider() != ref.Provider() {
		return nil, nil, affinity.NewError(affinity.RouteUnsupported, time.Time{})
	}
	return a, e, nil
}

func (m *Manager) coldAffinityAuth(ctx context.Context, providers []string, model string, opts exec.Options) (affinity.CredentialRef, error) {
	providerSet := make(map[string]bool)
	for _, p := range providers {
		providerSet[p] = true
	}
	forced := pinnedAuthIDFromMetadata(opts.Metadata)
	candidates := make([]domain.Candidate, 0)
	auths := make(map[string]*Auth)
	refs := make(map[string]affinity.CredentialRef)
	var rejected error
	for _, a := range m.List() {
		if !providerSet[a.Provider] || (forced != "" && a.ID != forced) {
			continue
		}
		ref, err := m.credentialRef(a)
		if err != nil {
			rejected = err
			continue
		}
		if _, _, err = m.validateAffinityAuth(ctx, ref, model, opts); err != nil {
			rejected = err
			continue
		}
		candidate, err := domain.NewCandidate(a.ID, authPriority(a), float64(authWeight(a)), m.affinityView(ref, model))
		if err != nil {
			return ref, err
		}
		candidates = append(candidates, candidate)
		auths[a.ID] = a
		refs[a.ID] = ref
	}
	admission := domain.Rank(candidates)
	if winner := admission.Winner(); winner != "" {
		return refs[winner], nil
	}
	fallback := make([]*Auth, 0)
	for _, id := range admission.Fallback() {
		fallback = append(fallback, auths[id])
	}
	if len(fallback) == 0 {
		if rejected != nil {
			return affinity.CredentialRef{}, rejected
		}
		return affinity.CredentialRef{}, affinity.NewError(affinity.RouteUnsupported, time.Time{})
	}
	sort.Slice(fallback, func(i, j int) bool { return fallback[i].ID < fallback[j].ID })
	selector := m.Selector()
	if !isBuiltInSelector(selector) {
		return affinity.CredentialRef{}, affinity.NewError(affinity.RouteUnsupported, time.Time{})
	}
	selected, err := selector.Pick(ctx, "mixed", model, opts, fallback)
	if err != nil {
		return affinity.CredentialRef{}, err
	}
	if selected == nil || refs[selected.ID].AuthID() == "" {
		return affinity.CredentialRef{}, affinity.NewError(affinity.RouteUnsupported, time.Time{})
	}
	return refs[selected.ID], nil
}

func (m *Manager) beginAffinity(ctx context.Context, providers []string, req exec.Request, opts exec.Options, count bool) (context.Context, exec.Options, affinity.Binding, func(), error) {
	var empty affinity.Binding
	failure := func(err error) (context.Context, exec.Options, affinity.Binding, func(), error) {
		return ctx, opts, empty, nil, err
	}
	if err := opts.Affinity.Validate(); err != nil {
		return failure(affinity.NewError(affinity.IdentityInvalid, time.Time{}))
	}
	if count && opts.Affinity.Intent() != affinity.ContinueIntent {
		return failure(affinity.NewError(affinity.BindingMissing, time.Time{}))
	}
	if m.HomeEnabled() || m.hasPluginScheduler() || !isBuiltInSelector(m.Selector()) {
		return failure(affinity.NewError(affinity.RouteUnsupported, time.Time{}))
	}
	m.affinityMu.Lock()
	r := m.affinityRuntime
	if r == nil {
		m.affinityMu.Unlock()
		return failure(affinity.NewError(affinity.RouteUnsupported, time.Time{}))
	}
	key := opts.Affinity.Key()
	if r.busy[key] {
		m.affinityMu.Unlock()
		return failure(affinity.NewError(affinity.Busy, time.Time{}))
	}
	r.busy[key] = true
	m.affinityMu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { m.affinityMu.Lock(); delete(r.busy, key); m.affinityMu.Unlock() }) }
	fail := func(err error) (context.Context, exec.Options, affinity.Binding, func(), error) {
		release()
		return failure(err)
	}
	// strict lookup precedes candidate pruning so an unavailable saved chat cannot become cold
	binding, found, err := r.store.Lookup(ctx, key)
	if err != nil {
		return fail(err)
	}
	now := r.now()
	if found && !binding.ExpiresAt().After(now) {
		return fail(affinity.NewError(affinity.BindingExpired, time.Time{}))
	}
	if opts.Affinity.Intent() == affinity.ContinueIntent {
		if !found {
			return fail(affinity.NewError(affinity.BindingMissing, time.Time{}))
		}
		if binding.Handle() != opts.Affinity.Handle() {
			return fail(affinity.NewError(affinity.BindingConflict, time.Time{}))
		}
	}
	ref := binding.Credential()
	if !found {
		ref, err = m.coldAffinityAuth(ctx, providers, req.Model, opts)
		if err != nil {
			return fail(err)
		}
	}
	if ref.Provider() == "claude" {
		opts.NativeHistory = exec.NativeHistoryPreserve
	}
	allowed := false
	for _, p := range providers {
		if p == ref.Provider() {
			allowed = true
		}
	}
	if !allowed {
		return fail(affinity.NewError(affinity.ModelUnsupported, time.Time{}))
	}
	if forced := pinnedAuthIDFromMetadata(opts.Metadata); forced != "" && forced != ref.AuthID() {
		return fail(affinity.NewError(affinity.BindingConflict, time.Time{}))
	}
	a, providerExecutor, err := m.validateAffinityAuth(ctx, ref, req.Model, opts)
	if err != nil {
		return fail(err)
	}
	if ref.Provider() == "claude" {
		models, _, _, routing := m.preparedExecutionModelsWithAlias(a, req.Model)
		if errPreflight := m.preflightNativeHistoryModels(ctx, providerExecutor, a, req, opts, req.Model, models, routing); errPreflight != nil {
			return fail(errPreflight)
		}
	}
	if !found {
		handle, errHandle := affinity.NewHandle()
		if errHandle != nil {
			return fail(affinity.NewError(affinity.StoreUnavailable, time.Time{}))
		}
		binding, err = affinity.NewBinding(key, handle, ref, now, now, r.retention.Expiration(now, now))
		if err != nil {
			return fail(err)
		}
		var created bool
		binding, created, err = r.store.CreateIfAbsent(ctx, binding)
		if err != nil {
			return fail(err)
		}
		if !created {
			return fail(affinity.NewError(affinity.BindingConflict, time.Time{}))
		}
	} else {
		binding, err = r.store.Touch(ctx, key, binding.Handle(), now)
		if err != nil {
			return fail(err)
		}
	}
	opts.Metadata = maps.Clone(opts.Metadata)
	opts.EnsureMetadata()[exec.PinnedAuthMetadataKey] = ref.AuthID()
	opts.Headers = affinity.StripHeaders(opts.Headers)
	if opts.AffinityBound != nil {
		opts.AffinityBound(binding.Handle())
	}
	ctx = context.WithValue(ctx, affinityPinKey{}, affinityPin{ref})
	return ctx, opts, binding, release, nil
}

// Execute enforces optional durable affinity at the manager execution boundary
func (m *Manager) Execute(ctx context.Context, providers []string, req exec.Request, opts exec.Options) (exec.Response, error) {
	if !opts.RequestHooksAllowed() && opts.RequestAfterAuthInterceptor != nil {
		return exec.Response{}, affinity.NewError(affinity.PrefixTransformUnsupported, time.Time{})
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.NativeHistory == exec.NativeHistoryPreserve && (m.HomeEnabled() || m.hasPluginScheduler()) {
		return exec.Response{}, affinity.NewError(affinity.RouteUnsupported, time.Time{})
	}
	if opts.Affinity.Intent() == affinity.Legacy {
		return m.executeLegacy(ctx, providers, req, opts)
	}
	ctx, opts, b, release, err := m.beginAffinity(ctx, providers, req, opts, false)
	if err != nil {
		return exec.Response{}, err
	}
	defer release()
	resp, err := m.executeLegacy(ctx, []string{b.Credential().Provider()}, req, opts)
	if err == nil {
		resp.Headers = affinity.ResponseHeaders(resp.Headers, b.Handle())
	}
	return resp, err
}

// ExecuteCount requires continuation and never creates a cold generation binding
func (m *Manager) ExecuteCount(ctx context.Context, providers []string, req exec.Request, opts exec.Options) (exec.Response, error) {
	if !opts.RequestHooksAllowed() && opts.RequestAfterAuthInterceptor != nil {
		return exec.Response{}, affinity.NewError(affinity.PrefixTransformUnsupported, time.Time{})
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.NativeHistory == exec.NativeHistoryPreserve && (m.HomeEnabled() || m.hasPluginScheduler()) {
		return exec.Response{}, affinity.NewError(affinity.RouteUnsupported, time.Time{})
	}
	if opts.Affinity.Intent() == affinity.Legacy {
		return m.executeCountLegacy(ctx, providers, req, opts)
	}
	ctx, opts, b, release, err := m.beginAffinity(ctx, providers, req, opts, true)
	if err != nil {
		return exec.Response{}, err
	}
	defer release()
	resp, err := m.executeCountLegacy(ctx, []string{b.Credential().Provider()}, req, opts)
	if err == nil {
		resp.Headers = affinity.ResponseHeaders(resp.Headers, b.Handle())
	}
	return resp, err
}

// ExecuteStream holds one chat turn until upstream completion or cancellation
func (m *Manager) ExecuteStream(ctx context.Context, providers []string, req exec.Request, opts exec.Options) (*exec.StreamResult, error) {
	if !opts.RequestHooksAllowed() && opts.RequestAfterAuthInterceptor != nil {
		return nil, affinity.NewError(affinity.PrefixTransformUnsupported, time.Time{})
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.NativeHistory == exec.NativeHistoryPreserve && (m.HomeEnabled() || m.hasPluginScheduler()) {
		return nil, affinity.NewError(affinity.RouteUnsupported, time.Time{})
	}
	if opts.Affinity.Intent() == affinity.Legacy {
		return m.executeStreamLegacy(ctx, providers, req, opts)
	}
	ctx, opts, b, release, err := m.beginAffinity(ctx, providers, req, opts, false)
	if err != nil {
		return nil, err
	}
	dispatchCtx, cancel := context.WithCancel(ctx)
	stream, err := m.executeStreamLegacy(dispatchCtx, []string{b.Credential().Provider()}, req, opts)
	if err != nil {
		cancel()
		release()
		return nil, err
	}
	if stream == nil || stream.Chunks == nil {
		cancel()
		release()
		return nil, affinity.NewError(affinity.StoreUnavailable, time.Time{})
	}
	out := make(chan exec.StreamChunk)
	go func() {
		defer close(out)
		defer release()
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-stream.Chunks:
				if !ok {
					return
				}
				select {
				case <-ctx.Done():
					return
				case out <- chunk:
				}
				if chunk.Err != nil {
					return
				}
			}
		}
	}()
	return &exec.StreamResult{Headers: affinity.ResponseHeaders(stream.Headers, b.Handle()), Chunks: out}, nil
}

func (m *Manager) pickAffinity(ctx context.Context, pin affinityPin, model string, opts exec.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if _, used := tried[pin.ref.AuthID()]; used {
		return nil, nil, "", affinity.NewError(affinity.CredentialCooldown, time.Time{})
	}
	a, e, err := m.validateAffinityAuth(ctx, pin.ref, model, opts)
	if err != nil {
		return nil, nil, "", err
	}
	return a, &affinityExecutor{manager: m, delegate: e, ref: pin.ref, model: model, opts: opts}, pin.ref.Provider(), nil
}

// QuotaLease is a fenced snapshot of the credential owner at acquisition time
type QuotaLease struct {
	ref               affinity.CredentialRef
	epoch, generation uint64
}

// QuotaLease captures exact lineage and runtime version without exporting token material
func (m *Manager) QuotaLease(authID string) (QuotaLease, error) {
	a, ok := m.GetByID(authID)
	if !ok {
		return QuotaLease{}, affinity.NewError(affinity.CredentialMissing, time.Time{})
	}
	ref, err := m.credentialRef(a)
	return QuotaLease{ref, a.RegistrationEpoch, a.Generation}, err
}

// quotaLeaseForSnapshot never attaches a replacement lease to selected execution auth
func (m *Manager) quotaLeaseForSnapshot(a *Auth) (QuotaLease, error) {
	if a == nil {
		return QuotaLease{}, affinity.NewError(affinity.CredentialMissing, time.Time{})
	}
	ref, err := m.credentialRef(a)
	if err != nil {
		return QuotaLease{}, err
	}
	lease := QuotaLease{ref, a.RegistrationEpoch, a.Generation}
	current, err := m.QuotaLease(a.ID)
	if err != nil || current != lease {
		return QuotaLease{}, affinity.NewError(affinity.CredentialChanged, time.Time{})
	}
	return lease, nil
}

// Credential returns the immutable principal reference, not a provider token
func (l QuotaLease) Credential() affinity.CredentialRef { return l.ref }

// ObserveQuota validates owner, receipt and generation before replacing any window
func (m *Manager) ObserveQuota(lease QuotaLease, observation domain.Snapshot) bool {
	if observation.Credential() != lease.ref || observation.ObservedAt().IsZero() {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.auths[lease.ref.AuthID()]
	if a == nil || a.RegistrationEpoch != lease.epoch || a.Generation != lease.generation || a.Disabled || a.Status == StatusDisabled {
		return false
	}
	// credentialRef takes only affinityMu after GetByID has released manager ownership elsewhere
	ref, err := m.credentialRef(a)
	if err != nil || ref != lease.ref {
		return false
	}
	m.affinityMu.Lock()
	defer m.affinityMu.Unlock()
	r := m.affinityRuntime
	if r == nil || observation.ObservedAt().After(r.now()) {
		return false
	}
	record := r.observations[a.ID]
	if record.ref != ref {
		record = quotaRecord{ref: ref, windows: make(map[string]domain.Snapshot)}
	}
	var accepted []domain.Window
	for _, w := range observation.Windows() {
		old, ok := record.windows[w.ID()]
		if ok && !observation.ObservedAt().After(old.ObservedAt()) {
			continue
		}
		if !ok && len(record.windows) >= 32 {
			continue
		}
		record.windows[w.ID()] = observation.WindowSnapshot(w)
		accepted = append(accepted, w)
	}
	r.observations[a.ID] = record
	// recording under these locks only touches memory and a non-blocking queue send,
	// which keeps history in receipt order; the writer goroutine does all file I/O
	r.history.record(a.ID, ref.Provider(), observation.ObservedAt(), accepted)
	return len(accepted) > 0
}

// PollQuotaOnce performs a bounded read-only pass through authenticated provider transports
// Failures preserve original observations and timestamps
func (m *Manager) PollQuotaOnce(ctx context.Context) {
	m.affinityMu.Lock()
	r := m.affinityRuntime
	m.affinityMu.Unlock()
	if r == nil {
		return
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < r.settings.PollConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				m.pollQuotaCredential(ctx, id, r)
			}
		}()
	}
	for _, a := range m.List() {
		if a.AuthKind() != AuthKindOAuth || a.Disabled || a.Status == StatusDisabled {
			continue
		}
		select {
		case jobs <- a.ID:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
	m.affinityMu.Lock()
	idle := len(r.busy) == 0
	m.affinityMu.Unlock()
	if idle {
		_ = r.store.Prune(ctx, r.now())
	}
	r.history.maybeCompact()
}

type quotaHTTPDoer struct {
	manager  *Manager
	lease    QuotaLease
	delegate ProviderExecutor
	auth     *Auth
}

func (d quotaHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	lease, err := d.manager.QuotaLease(d.auth.ID)
	if err != nil || lease != d.lease {
		return nil, affinity.NewError(affinity.CredentialChanged, time.Time{})
	}
	return d.delegate.HttpRequest(req.Context(), d.auth, req)
}
func (m *Manager) pollQuotaCredential(ctx context.Context, id string, r *affinityRuntime) {
	if ctx.Err() != nil {
		return
	}
	m.affinityMu.Lock()
	if r.polling[id] {
		m.affinityMu.Unlock()
		return
	}
	r.polling[id] = true
	m.affinityMu.Unlock()
	defer func() { m.affinityMu.Lock(); delete(r.polling, id); m.affinityMu.Unlock() }()
	select {
	case r.pollSlots <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-r.pollSlots }()
	if ctx.Err() != nil {
		return
	}
	a, ok := m.GetByID(id)
	if !ok || a.Disabled || a.Status == StatusDisabled {
		return
	}
	lease, err := m.quotaLeaseForSnapshot(a)
	if err != nil {
		return
	}
	e, ok := m.Executor(a.Provider)
	if !ok {
		return
	}
	authority, local := e.(exec.LocalExecutionAuthority)
	if !local || authority.LocalBuiltinProvider() != a.Provider || m.HomeEnabled() || m.hasPluginScheduler() {
		return
	}
	ctx = exec.WithCredentialFence(ctx, func(request *http.Request) error {
		currentAuth, ok := m.GetByID(id)
		if !ok || currentAuth.Disabled || currentAuth.Status == StatusDisabled || m.HomeEnabled() || m.hasPluginScheduler() {
			return affinity.NewError(affinity.CredentialDisabled, time.Time{})
		}
		current, err := m.QuotaLease(id)
		if err != nil || current != lease {
			return affinity.NewError(affinity.CredentialChanged, time.Time{})
		}
		return checkAffinityCredentialRequest(a, request, true)
	})
	if rt := m.roundTripperFor(a); rt != nil {
		ctx = context.WithValue(ctx, roundTripperContextKey{}, rt)
		ctx = context.WithValue(ctx, "cliproxy.roundtripper", rt)
	}
	account, _ := a.Metadata["account_id"].(string)
	s, err := runtimequota.FetchUsage(ctx, quotaHTTPDoer{m, lease, e, a}, lease.ref, account)
	if err == nil {
		m.ObserveQuota(lease, s)
	}
}

// RunQuotaPoller performs an initial pass and periodic cancellable read-only passes
func (m *Manager) RunQuotaPoller(ctx context.Context) {
	m.affinityMu.Lock()
	r := m.affinityRuntime
	m.affinityMu.Unlock()
	if r == nil {
		return
	}
	ticker := time.NewTicker(r.settings.PollInterval)
	defer ticker.Stop()
	m.runQuotaPoller(ctx, ticker.C)
}

func (m *Manager) runQuotaPoller(ctx context.Context, ticks <-chan time.Time) {
	m.PollQuotaOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			m.PollQuotaOnce(ctx)
		}
	}
}

// ValidateChatRequest checks saved identity before HTTP model-route pruning
// It does not acquire a turn, create a binding, or extend retention
func (m *Manager) ValidateChatRequest(ctx context.Context, request affinity.Request) error {
	if request.Intent() == affinity.Legacy || request.Validate() != nil {
		return affinity.NewError(affinity.IdentityInvalid, time.Time{})
	}
	m.affinityMu.Lock()
	r := m.affinityRuntime
	m.affinityMu.Unlock()
	if r == nil {
		return affinity.NewError(affinity.RouteUnsupported, time.Time{})
	}
	b, found, err := r.store.Lookup(ctx, request.Key())
	if err != nil {
		return err
	}
	if !found {
		if request.Intent() == affinity.StartIntent {
			return nil
		}
		return affinity.NewError(affinity.BindingMissing, time.Time{})
	}
	if !b.ExpiresAt().After(r.now()) {
		return affinity.NewError(affinity.BindingExpired, time.Time{})
	}
	if request.Intent() == affinity.ContinueIntent && b.Handle() != request.Handle() {
		return affinity.NewError(affinity.BindingConflict, time.Time{})
	}
	_, providerExecutor, err := m.validateAffinityAuth(ctx, b.Credential(), "", exec.Options{})
	if err != nil {
		return err
	}
	if b.Credential().Provider() == "claude" {
		if _, ok := providerExecutor.(exec.NativeHistoryPreserver); !ok {
			return affinity.NewError(affinity.PrefixTransformUnsupported, time.Time{})
		}
	}
	return nil
}

// ValidateChatContinuation checks a saved local capability before a transport upgrade
// Dispatch still requires the manager turn guard and exact model eligibility
func (m *Manager) ValidateChatContinuation(ctx context.Context, request affinity.Request) error {
	if request.Intent() != affinity.ContinueIntent {
		return affinity.NewError(affinity.IdentityInvalid, time.Time{})
	}
	return m.ValidateChatRequest(ctx, request)
}

func (m *Manager) affinityNow() time.Time {
	m.affinityMu.Lock()
	defer m.affinityMu.Unlock()
	if m.affinityRuntime == nil {
		return time.Now()
	}
	return m.affinityRuntime.now()
}

// QuotaView returns the manager's receipt-time view for an exact current credential
// It does not change cooldowns, fetch data, or expose token material
func (m *Manager) QuotaView(authID, model string) (domain.View, error) {
	lease, err := m.QuotaLease(authID)
	if err != nil {
		return domain.View{}, err
	}
	return m.affinityView(lease.ref, model), nil
}

func (m *Manager) preflightNativeHistory(ctx context.Context, providerExecutor ProviderExecutor, a *Auth, req exec.Request, opts exec.Options) error {
	if a.AuthKind() == AuthKindAPIKey {
		if scoped, ok := providerExecutor.(APIKeyConfigExecutor); ok {
			providerExecutor = scoped.ForAPIKey()
		}
	}
	preserver, ok := providerExecutor.(exec.NativeHistoryPreserver)
	if !ok {
		return affinity.NewError(affinity.PrefixTransformUnsupported, time.Time{})
	}
	return preserver.PreflightNativeHistory(ctx, req, opts, exec.NativeHistoryCredential{Attributes: maps.Clone(a.Attributes), Metadata: maps.Clone(a.Metadata)})
}

func (m *Manager) preflightNativeHistoryModels(ctx context.Context, providerExecutor ProviderExecutor, a *Auth, req exec.Request, opts exec.Options, routeModel string, models []string, routing *apiKeyModelRoutingSnapshot) error {
	if len(models) == 0 {
		return affinity.NewError(affinity.ModelUnsupported, time.Time{})
	}
	for _, model := range models {
		attempt := req
		attempt.Model = model
		attempt = attachResolvedExecutionModelInfo(routing, attempt, a, routeModel, model, false)
		if errPreflight := m.preflightNativeHistory(ctx, providerExecutor, a, attempt, opts); errPreflight != nil {
			return errPreflight
		}
	}
	return nil
}

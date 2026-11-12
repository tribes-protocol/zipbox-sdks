package zipbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"
)

// Status is a box's lifecycle state.
type Status string

// Box lifecycle states. StatusPaused: its Pause took effect and nothing is billed;
// Resume brings it back with its files and installed software. StatusStopping is a
// Pause still landing.
const (
	StatusProvisioning Status = "provisioning"
	StatusRunning      Status = "running"
	StatusStopping     Status = "stopping"
	StatusPaused       Status = "paused"
	StatusDeleting     Status = "deleting" // signing out of its tailnet before it is destroyed
	StatusDestroyed    Status = "destroyed"
)

// Phase is what a box is doing now. Kept a string: a later value must not break parsing.
type Phase string

// Box phases. Transition (on the full row) says how far a change has got.
const (
	PhaseStarting     Phase = "starting"
	PhaseRunning      Phase = "running"
	PhaseStopping     Phase = "stopping"
	PhasePaused       Phase = "paused"
	PhaseResizing     Phase = "resizing"
	PhaseForking      Phase = "forking"
	PhaseDeleting     Phase = "deleting"
	PhaseDeleteFailed Phase = "delete_failed" // a delete was refused; the box still runs
	PhaseStartFailed  Phase = "start_failed"
	PhaseGone         Phase = "gone"
)

// Readiness is whether a box will answer: what every wait reads.
type Readiness string

// Box readiness. Every transient (a host offline, no room, an upgrade waiting to
// restore) is ReadinessSettling. Paused, StartFailed, Deleting and Gone will not
// become ready by themselves.
const (
	ReadinessReady         Readiness = "ready"
	ReadinessSettling      Readiness = "settling"
	ReadinessAwaitsTailnet Readiness = "awaits_tailnet"
	ReadinessPaused        Readiness = "paused"
	ReadinessStartFailed   Readiness = "start_failed"
	ReadinessDeleting      Readiness = "deleting"
	ReadinessGone          Readiness = "gone"
)

// Action is a lifecycle verb a box can accept.
type Action string

// Lifecycle verbs, as AllowedActions names them.
const (
	ActionResume           Action = "resume"
	ActionPause            Action = "pause"
	ActionRestart          Action = "restart"
	ActionDelete           Action = "delete"
	ActionCancelTransition Action = "cancel_transition"
	ActionFork             Action = "fork"
)

// MachineSize is the resolved size a box runs at.
type MachineSize struct {
	VCPUs  int `json:"vcpus"`
	MemMiB int `json:"memMib"`
	DiskGB int `json:"diskGb"`
}

// Sandbox is the documented subset of a box row.
type Sandbox struct {
	ID          string       `json:"id"`
	Name        *string      `json:"name,omitempty"`
	Domain      *string      `json:"domain,omitempty"`
	IPv6        *string      `json:"ipv6,omitempty"`
	Status      Status       `json:"status"`
	Tier        *string      `json:"tier,omitempty"`
	PendingTier *string      `json:"pendingTier,omitempty"`
	MachineSize *MachineSize `json:"machineSize,omitempty"`
	// IsArchived is true when the box has a backup in cold storage or one is in flight.
	IsArchived     *bool      `json:"isArchived,omitempty"`
	CPUPct         *float64   `json:"cpuPct,omitempty"`
	MemPct         *float64   `json:"memPct,omitempty"`
	DiskPct        *float64   `json:"diskPct,omitempty"`
	NetRxBps       *float64   `json:"netRxBps,omitempty"`
	NetTxBps       *float64   `json:"netTxBps,omitempty"`
	StatsUpdatedAt *time.Time `json:"statsUpdatedAt,omitempty"`
	StartFailedAt  *time.Time `json:"startFailedAt,omitempty"`
	ClaimedAt      *time.Time `json:"claimedAt,omitempty"`
	// PrivateMode is true when the box answers only devices on your tailnet.
	PrivateMode bool `json:"privateMode"`
	// Tailscale is the box's tailnet status; nil when it never joined one.
	Tailscale *TailscaleStatus `json:"tailscale,omitempty"`
	// TailscaleSupported is false when the box's ZipOS predates Tailscale
	// (Tailscale.Connect fails with CodeTailscaleRequiresUpdate until the ZipOS
	// update is accepted); nil until the box has reported since it booted.
	TailscaleSupported *bool `json:"tailscaleSupported,omitempty"`
	// AgentPublicKey is the box's own P-256 key (base64 SPKI DER) that box
	// credentials are sealed to. Nil until the box has started; a claim re-mints it.
	AgentPublicKey *string `json:"agentPublicKey,omitempty"`
	// Fork is the fork still making this box: its phase, source recovery, waits and
	// error. Nil on every box that is not being forked into.
	Fork *SandboxFork `json:"fork,omitempty"`
	// Phase is what the box is doing now. Empty from an older control plane.
	Phase Phase `json:"phase,omitempty"`
	// AllowedActions are the lifecycle verbs the box accepts now. A listed verb can
	// still be refused for a reason outside the box (credit, capacity, an argument).
	// Nil from an older control plane.
	AllowedActions []Action  `json:"allowedActions,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// SandboxDetail is one box as GET /sandbox/{id} returns it.
type SandboxDetail struct {
	Sandbox
	// Ready is true once the box is running and serving its owner.
	Ready bool `json:"ready"`
	// Readiness is whether the box will answer, from the same connection gate as
	// Ready: what every wait reads. Empty from an older control plane, where the SDK
	// falls back to Ready and Status.
	Readiness Readiness `json:"readiness,omitempty"`
	// HTTPSReady is true once BaseURL completes a TLS handshake with a trusted
	// certificate. A fresh name's certificate can trail Ready; the SDK's box calls
	// retry the handshake meanwhile.
	HTTPSReady bool `json:"httpsReady"`
	// BaseURL is the box's HTTPS origin once Ready.
	BaseURL *string `json:"baseUrl,omitempty"`
	// BoxToken is the per-key, per-box bearer the box accepts. The SDK manages
	// it; it is exposed for curl and debugging.
	BoxToken *string `json:"boxToken,omitempty"`
}

// MarshalJSON leaves BoxToken out: it is a credential, and a detail printed or
// logged as JSON (the CLI's --json) must never carry it.
func (d SandboxDetail) MarshalJSON() ([]byte, error) {
	type plain SandboxDetail
	p := plain(d)
	p.BoxToken = nil
	return json.Marshal(p)
}

// CreateOptions configures a new box. GetOrCreate takes the same options and
// applies them only when it creates the box.
type CreateOptions struct {
	// Name is the box's permanent subdomain. Empty = the account's default box.
	Name string
	// Tier is the machine size (xs, s, m, l, xl, xxl). Empty = the default tier.
	Tier string
	// SSHPublicKey is an OpenSSH public key line authorized on the box.
	SSHPublicKey string
	// Mailbox creates the box's mailbox now. False = created the first time the box uses mail.
	Mailbox bool
	// Private starts the box in private mode ("tailnet only"): it is never public.
	// A private box is not ready until it joins your tailnet, so Create returns
	// once it is running with its key; then call Box.Tailscale.Connect.
	Private bool
}

// SandboxesService manages boxes. Reach it as Client.Sandboxes.
type SandboxesService struct {
	client *Client
}

type createBody struct {
	Name         string `json:"name,omitempty"`
	Tier         string `json:"tier,omitempty"`
	SSHPublicKey string `json:"sshPublicKey,omitempty"`
	Mailbox      bool   `json:"mailbox,omitempty"`
	Private      bool   `json:"private,omitempty"`
}

type createResponse struct {
	SandboxID string `json:"sandboxId"`
	Created   bool   `json:"created"`
}

// Create creates a box and returns it once it is ready: at once when the server
// bound a booted machine inside the request (its answer is ready), else after
// waiting as WaitReady does. The wait has no deadline of its own; bound it with ctx.
// The request carries an Idempotency-Key, so a retried create never makes two
// boxes. With Private set it waits until the box is running with its key instead.
func (s *SandboxesService) Create(ctx context.Context, opts CreateOptions) (*Box, error) {
	var raw json.RawMessage
	headers := map[string]string{
		"Idempotency-Key": newID(),
		"Accept":          claimStreamMediaType + ", application/json",
	}
	body := createBody{Name: opts.Name, Tier: opts.Tier, SSHPublicKey: opts.SSHPublicKey, Mailbox: opts.Mailbox, Private: opts.Private}
	// The answer streamed (conformance README, streamed create); its placed line is
	// never dialled. An unstreamed answer reads as before.
	if err := s.client.apiUnwrap(ctx, "POST", "/sandbox/identity/claim", body, &raw, true, headers, readClaimStream); err != nil {
		return nil, err
	}
	var created createResponse
	if err := json.Unmarshal(raw, &created); err != nil {
		return nil, err
	}
	if detail := claimedDetail(created.SandboxID, raw); detail != nil {
		return s.readyBox(created.SandboxID, detail), nil
	}
	return s.WaitReady(ctx, created.SandboxID)
}

// claimedDetail is the box a create answer carries when the server bound it inside
// the request (ready, with its baseUrl and box token), or nil when it must be
// waited on. An answer without the detail fields (an older control plane) is nil.
func claimedDetail(id string, raw json.RawMessage) *SandboxDetail {
	var probe struct {
		Ready *bool `json:"ready"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.Ready == nil {
		return nil
	}
	var detail SandboxDetail
	if json.Unmarshal(raw, &detail) != nil {
		return nil
	}
	detail.ID = id
	if detail.readiness() != ReadinessReady || detail.BaseURL == nil {
		return nil
	}
	return &detail
}

// readiness is the box's Readiness as the server decided it. A control plane that
// predates the field gets the rule before it: ready, private and waiting for a
// tailnet, ended (destroyed, deleting), or still settling. The fallback can
// be removed once every control plane sends readiness.
func (d *SandboxDetail) readiness() Readiness {
	switch d.Readiness {
	case ReadinessReady, ReadinessSettling, ReadinessAwaitsTailnet, ReadinessPaused,
		ReadinessStartFailed, ReadinessDeleting, ReadinessGone:
		return d.Readiness
	}
	return d.fallbackReadiness()
}

func (d *SandboxDetail) fallbackReadiness() Readiness {
	if d.Ready {
		return ReadinessReady
	}
	joined := d.Tailscale != nil && d.Tailscale.IPv4 != nil && *d.Tailscale.IPv4 != ""
	if d.PrivateMode && !joined && d.Status == StatusRunning && d.AgentPublicKey != nil && *d.AgentPublicKey != "" {
		return ReadinessAwaitsTailnet
	}
	switch d.Status {
	case StatusDestroyed:
		return ReadinessGone
	case StatusDeleting:
		return ReadinessDeleting
	case StatusPaused:
		return ReadinessPaused
	}
	return ReadinessSettling
}

// pausedRemedy is how to resume a paused box, unless the server says it cannot resume now.
func pausedRemedy(d *SandboxDetail) string {
	if d.AllowedActions == nil {
		return "resume it with Sandboxes.Resume"
	}
	for _, action := range d.AllowedActions {
		if action == ActionResume {
			return "resume it with Sandboxes.Resume"
		}
	}
	return "it cannot be resumed now; see its AllowedActions"
}

// endedError is the error a wait returns for a box that will not become ready by
// itself, or nil while it may. Each is an answered verdict, never retryable:
// nothing but the caller resumes the box.
func endedError(id string, d *SandboxDetail) error {
	var zerr *Error
	switch d.readiness() {
	case ReadinessPaused:
		zerr = newError(CodeBoxNotRunning, "box "+id+" is paused and nothing will resume it; "+pausedRemedy(d))
	case ReadinessStartFailed:
		zerr = newError(CodeStartFailed, "box "+id+" failed to start; "+pausedRemedy(d))
	case ReadinessDeleting, ReadinessGone:
		zerr = newError(CodeConflict, "box "+id+" is "+string(d.Status)+" and will not become ready")
	default:
		return nil
	}
	zerr.Retryable = false
	return zerr
}

// pauseEndedError is the error a wait for a Pause returns for a box that will not
// reach paused by itself, or nil while it may (paused, settling). A box that reads
// ready again had its Pause cancelled or refused; a deleted, gone or failed box
// will not pause. Each is an answered verdict, never retryable.
func pauseEndedError(id string, d *SandboxDetail) error {
	var zerr *Error
	switch readiness := d.readiness(); readiness {
	case ReadinessPaused, ReadinessSettling:
		return nil
	case ReadinessReady, ReadinessAwaitsTailnet:
		zerr = newError(CodeConflict, "box "+id+" is running: its pause was cancelled or did not take effect")
	default:
		zerr = newError(CodeConflict, "box "+id+" is "+string(readiness)+" and will not pause")
	}
	zerr.Retryable = false
	return zerr
}

// boxNotReadyError is the error a box call returns on a detail it cannot call: a
// private box that has not joined a tailnet answers only once the owner's auth key
// joins it (CodeTailnetNotConnected); anything else is CodeBoxNotRunning,
// retryable only while the box is settling.
func boxNotReadyError(id string, d *SandboxDetail) *Error {
	stated := d.readiness()
	if stated == ReadinessAwaitsTailnet {
		zerr := newError(CodeTailnetNotConnected, "box "+id+" is private and has not joined a tailnet yet; connect it with Box.Tailscale.Connect")
		zerr.Retryable = false
		return zerr
	}
	zerr := newError(CodeBoxNotRunning, "box "+id+" is "+string(d.Status)+", not ready; resume it with Sandboxes.Resume or wait")
	if d.Readiness == stated {
		zerr.Retryable = stated == ReadinessSettling
	} else {
		// Without readiness (an older control plane), the rule before it: booting or
		// restarting comes back by itself, and nothing else does.
		zerr.Retryable = d.Status == StatusProvisioning || d.Status == StatusRunning
	}
	return zerr
}

// WaitReady waits until a box is ready, on GET /sandbox/{id}/wait (the server
// answers when it is; an older API is polled with backoff instead), and returns
// its handle. The wait has no deadline of its own; bound it with ctx. A private
// box that has not joined a tailnet yet is returned once it is running with its
// key. A box that will not become ready by itself is an error: paused
// CodeBoxNotRunning, a failed start CodeStartFailed, deleting or gone
// CodeConflict, none of them retryable.
func (s *SandboxesService) WaitReady(ctx context.Context, id string) (*Box, error) {
	detail, err := s.waitSettled(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.readyBox(id, detail), nil
}

// waitTarget is what a wait waits for, the `until` of GET /sandbox/{id}/wait: the
// box answering (untilReady), or its Pause taking effect (untilPaused).
type waitTarget string

const (
	untilReady  waitTarget = "ready"
	untilPaused waitTarget = "paused"
)

// settle is one read inside a wait: the detail once the wait is over, nil while the
// box may still get there, or the answered verdict of a box that will not get there
// by itself. A wait for ready also ends on awaits_tailnet (a private box running
// with its key: the caller seals an auth key to it).
func settle(id string, d *SandboxDetail, until waitTarget) (*SandboxDetail, error) {
	if until == untilReady {
		if err := endedError(id, d); err != nil {
			return nil, err
		}
		if d.readiness() == ReadinessSettling {
			return nil, nil
		}
		return d, nil
	}
	if err := pauseEndedError(id, d); err != nil {
		return nil, err
	}
	if d.readiness() == ReadinessPaused {
		return d, nil
	}
	return nil, nil
}

// waitSettled waits until the box is ready or awaits its tailnet, and returns that
// last read: what Create, GetOrCreate, WaitReady, Fork and a re-resolve wait on.
func (s *SandboxesService) waitSettled(ctx context.Context, id string) (*SandboxDetail, error) {
	return s.waitUntil(ctx, id, untilReady)
}

// remember caches the box target a ready read names; a box that is not ready no
// longer answers where it did, so its cached credentials go.
func (s *SandboxesService) remember(id string, detail *SandboxDetail) {
	if s.client.storeCreds(id, detail) == nil {
		s.client.forget(id)
	}
}

// readWait is one GET /sandbox/{id}/wait?until=...: the server holds it until the
// box reaches until, cannot reach it by itself, or its hold limit passes.
func (s *SandboxesService) readWait(ctx context.Context, id string, until waitTarget) (*SandboxDetail, error) {
	var out SandboxDetail
	if err := s.client.api(ctx, "GET", "/sandbox/"+url.PathEscape(id)+"/wait?until="+string(until), nil, &out, true, nil); err != nil {
		return nil, err
	}
	s.remember(id, &out)
	return &out, nil
}

// waitUntil waits until the box reaches until, on GET /sandbox/{id}/wait: the
// server holds the request until the box gets there (pushed when the host answers,
// no polling), until it cannot get there by itself, or until its hold limit passes.
// A held answer that is still settling is asked again at once: the server did the
// waiting. A read that fails with a retryable error goes on the poll ladder, never
// sooner than its RetryAfter. An API that predates /wait answers it 404 on a box
// that exists: then this is the poll on GET /sandbox/{id} it replaced, whose 404 is
// the box's own. The wait has no deadline of its own; ctx bounds it.
func (s *SandboxesService) waitUntil(ctx context.Context, id string, until waitTarget) (*SandboxDetail, error) {
	for attempt := 0; ; {
		detail, err := s.readWait(ctx, id, until)
		if err == nil {
			done, verdict := settle(id, detail, until)
			if verdict != nil {
				return nil, verdict
			}
			if done != nil {
				return done, nil
			}
			attempt = 0
			continue
		}
		zerr, ok := asError(err)
		if ok && zerr.Code == CodeNotFound {
			return s.pollUntil(ctx, id, until)
		}
		if !ok || !zerr.Retryable {
			return nil, err
		}
		wait := s.client.poll(attempt)
		attempt++
		if zerr.hasRetryAfter && zerr.RetryAfter > wait {
			wait = zerr.RetryAfter
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// pollUntil is the wait for an API without /wait: it re-reads GET /sandbox/{id}
// with backoff, never on a deadline, until the box reaches until or will not get
// there by itself. A read that fails with a retryable error (a control-plane
// deploy) goes on the same ladder, never sooner than its RetryAfter.
func (s *SandboxesService) pollUntil(ctx context.Context, id string, until waitTarget) (*SandboxDetail, error) {
	for attempt := 0; ; attempt++ {
		var retryAfter time.Duration
		detail, err := s.Get(ctx, id)
		if err != nil {
			zerr, ok := asError(err)
			if !ok || !zerr.Retryable {
				return nil, err
			}
			if zerr.hasRetryAfter {
				retryAfter = zerr.RetryAfter
			}
		} else {
			done, verdict := settle(id, detail, until)
			if verdict != nil {
				return nil, verdict
			}
			if done != nil {
				return done, nil
			}
		}
		wait := s.client.poll(attempt)
		if retryAfter > wait {
			wait = retryAfter
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// getInWait is one read of the box inside a wait. A read that fails with a
// retryable error (a control-plane deploy) is read again on the poll ladder, never
// sooner than its RetryAfter, with no ceiling; any other error is returned.
func (s *SandboxesService) getInWait(ctx context.Context, id string) (*SandboxDetail, error) {
	for attempt := 0; ; attempt++ {
		detail, err := s.Get(ctx, id)
		if err == nil {
			return detail, nil
		}
		zerr, ok := asError(err)
		if !ok || !zerr.Retryable {
			return nil, err
		}
		wait := s.client.poll(attempt)
		if zerr.hasRetryAfter && zerr.RetryAfter > wait {
			wait = zerr.RetryAfter
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// readyBox is the handle for a box a wait or a lifecycle answer returned.
func (s *SandboxesService) readyBox(id string, detail *SandboxDetail) *Box {
	box := s.client.Sandbox(id)
	s.client.storeCreds(id, detail)
	box.Detail = detail
	return box
}

// GetOrCreate returns the live box with this name, resuming it if it is paused,
// or creates it with opts. It waits until the box is ready; a private box that has
// not joined a tailnet yet is returned once it is running with its key. opts.Name
// must be empty or name (CodeInvalidRequest otherwise).
//
// The options apply only when a box is created. A box that already has this name
// is returned as it is, even when it differs (another tier, not private): that is
// not an error and nothing is resized. Check Box.Detail.Tier and call SetTier if
// it matters.
func (s *SandboxesService) GetOrCreate(ctx context.Context, name string, opts CreateOptions) (*Box, error) {
	if opts.Name != "" && opts.Name != name {
		return nil, newError(CodeInvalidRequest, "GetOrCreate("+name+") was given CreateOptions.Name "+opts.Name)
	}
	boxes, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, sb := range boxes {
		if sb.Name == nil || *sb.Name != name || sb.Status == StatusDestroyed || sb.Status == StatusDeleting {
			continue
		}
		// StatusStopping: a Pause is still landing; requestResume waits it out, then resumes.
		if sb.Status == StatusPaused || sb.Status == StatusStopping {
			accepted, err := s.requestResume(ctx, sb.ID)
			if err != nil {
				// The name was being deleted: it is gone now, so make it.
				if IsCode(err, CodeNotFound) {
					break
				}
				// Another caller resumed it first: the server's idempotent answer to a
				// second resume. A box now coming up is waited on like any other.
				if !IsCode(err, CodeConflict) {
					return nil, err
				}
				detail, getErr := s.getInWait(ctx, sb.ID)
				if getErr != nil {
					return nil, getErr
				}
				if detail.Status != StatusProvisioning && detail.Status != StatusRunning {
					return nil, err
				}
			} else if accepted != nil && accepted.readiness() == ReadinessReady {
				return s.readyBox(sb.ID, accepted), nil
			}
		}
		return s.WaitReady(ctx, sb.ID)
	}
	opts.Name = name
	return s.Create(ctx, opts)
}

// List returns the account's boxes.
func (s *SandboxesService) List(ctx context.Context) ([]Sandbox, error) {
	var out []Sandbox
	if err := s.client.api(ctx, "GET", "/sandbox", nil, &out, true, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// Get returns one box with its readiness and HTTPS origin.
func (s *SandboxesService) Get(ctx context.Context, id string) (*SandboxDetail, error) {
	var out SandboxDetail
	if err := s.client.api(ctx, "GET", "/sandbox/"+url.PathEscape(id), nil, &out, true, nil); err != nil {
		return nil, err
	}
	// A ready read is the box's current target: box calls use it from here. A box
	// that is not ready no longer answers where it did: its cached credentials go,
	// and the next box call reads them afresh.
	if s.client.storeCreds(id, &out) == nil {
		s.client.forget(id)
	}
	return &out, nil
}

// lifecycle sends a lifecycle verb. The answer is the box's detail: its final state
// (200) when the host answered within the server's budget, or its state in
// transition (202). Nil for an answer that carries no detail.
func (s *SandboxesService) lifecycle(ctx context.Context, id, action string) (*SandboxDetail, error) {
	s.client.forget(id)
	var raw json.RawMessage
	if err := s.client.api(ctx, "POST", "/sandbox/"+url.PathEscape(id)+"/"+action, nil, &raw, false, nil); err != nil {
		// An accepted verb with an empty body carries no detail.
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	var probe struct {
		ID     *string `json:"id"`
		Status *string `json:"status"`
		Ready  *bool   `json:"ready"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.ID == nil || probe.Status == nil || probe.Ready == nil {
		return nil, nil
	}
	var detail SandboxDetail
	if json.Unmarshal(raw, &detail) != nil {
		return nil, nil
	}
	s.remember(id, &detail)
	return &detail, nil
}

// finish is the accepted detail when it already shows until, else the wait for it.
func (s *SandboxesService) finish(ctx context.Context, id string, until waitTarget, accepted *SandboxDetail) (*SandboxDetail, error) {
	if accepted != nil && accepted.readiness() == Readiness(until) {
		return accepted, nil
	}
	return s.waitUntil(ctx, id, until)
}

// Pause pauses a box and returns it paused. Nothing is billed while it is paused,
// and Resume brings it back with its files and installed software. It returns at
// once when the server answers with the paused box, else waits for the Pause to
// take effect (no deadline of its own; bound it with ctx). A box that reads running
// again (the Pause was cancelled) is CodeConflict.
func (s *SandboxesService) Pause(ctx context.Context, id string) (*Box, error) {
	accepted, err := s.lifecycle(ctx, id, "pause")
	if err != nil {
		return nil, err
	}
	detail, err := s.finish(ctx, id, untilPaused, accepted)
	if err != nil {
		return nil, err
	}
	return s.readyBox(id, detail), nil
}

// Resume resumes a paused box and returns it ready. A box still StatusStopping is
// resumed once its Pause lands. It returns at once when the server answers with the
// ready box, else waits as WaitReady does. Credit-gated: insufficient_credits when
// dry.
func (s *SandboxesService) Resume(ctx context.Context, id string) (*Box, error) {
	accepted, err := s.requestResume(ctx, id)
	if err != nil {
		return nil, err
	}
	detail, err := s.finish(ctx, id, untilReady, accepted)
	if err != nil {
		return nil, err
	}
	return s.readyBox(id, detail), nil
}

// Stop is an alias of Pause: the same request and the same result.
func (s *SandboxesService) Stop(ctx context.Context, id string) (*Box, error) {
	return s.Pause(ctx, id)
}

// Start is an alias of Resume: the same request and the same result.
func (s *SandboxesService) Start(ctx context.Context, id string) (*Box, error) {
	return s.Resume(ctx, id)
}

// requestResume sends Resume and returns the detail it answered with (nil when the
// box came back by itself). A Pause the host has not answered yet leaves the box
// StatusStopping, and Resume answers conflict meanwhile. requestResume waits that
// out with backoff and resumes the box once it reads StatusPaused. A box that comes
// out of the wait already starting or running (a resize restores itself, a failed
// pause leaves it running) is not resumed again. Any other conflict is returned as
// it came.
//
// In parallel with the resume, never waited for, the box's last known origin is
// pre-connected (Client.preconnect), so the first call after it skips TCP and TLS.
func (s *SandboxesService) requestResume(ctx context.Context, id string) (*SandboxDetail, error) {
	origin, drop := s.client.preconnect(id)
	accepted, err := s.sendResume(ctx, id)
	if drop != nil && (err != nil || movedOrigin(accepted, origin)) {
		// A failed resume, or a box that moved (a name cutover, private mode): reach
		// it the normal way.
		drop()
	}
	return accepted, err
}

// movedOrigin reports whether a resume answered on another origin than the one
// pre-connected. An answer that names no origin yet moved nothing.
func movedOrigin(accepted *SandboxDetail, origin string) bool {
	return accepted != nil && accepted.BaseURL != nil && strings.TrimRight(*accepted.BaseURL, "/") != origin
}

// sendResume sends Resume, waiting out a Pause still landing (see requestResume).
func (s *SandboxesService) sendResume(ctx context.Context, id string) (*SandboxDetail, error) {
	waited := false
	for {
		accepted, err := s.lifecycle(ctx, id, "resume")
		var zerr *Error
		if !errors.As(err, &zerr) || zerr.Code != CodeConflict {
			return accepted, err
		}
		detail, getErr := s.getInWait(ctx, id)
		if getErr != nil {
			return nil, getErr
		}
		if detail.Status != StatusStopping {
			// A second conflict after a pause was waited out: the box came back by
			// itself, or it is paused and will not resume (that answer is final).
			if waited && (detail.Status == StatusProvisioning || detail.Status == StatusRunning) {
				return nil, nil
			}
			return nil, err
		}
		for attempt := 0; detail.Status == StatusStopping; attempt++ {
			if sleepErr := sleep(ctx, s.client.poll(attempt)); sleepErr != nil {
				return nil, sleepErr
			}
			if detail, getErr = s.getInWait(ctx, id); getErr != nil {
				return nil, getErr
			}
		}
		waited = true
		switch detail.Status {
		case StatusPaused:
		case StatusProvisioning, StatusRunning:
			return nil, nil
		default:
			return nil, err
		}
	}
}

// Restart hard-restarts a running box. Files survive; running processes do not.
func (s *SandboxesService) Restart(ctx context.Context, id string) error {
	s.client.forget(id)
	return s.client.api(ctx, "POST", "/sandbox/"+url.PathEscape(id)+"/reset", nil, nil, false, nil)
}

// TransitionCancelOutcome is what CancelTransition did.
type TransitionCancelOutcome string

const (
	// TransitionCleared: nothing was executing on a host; the queued change was
	// dropped and the box is already back where it was.
	TransitionCleared TransitionCancelOutcome = "cleared"
	// TransitionCancelling: the host is executing the change; its abort is on the way.
	TransitionCancelling TransitionCancelOutcome = "cancelling"
	// TransitionNone: the box had no change in flight.
	TransitionNone TransitionCancelOutcome = "none"
)

// CancelTransition cancels the box's state change in flight (a pause, a resume, a
// restart, a tier change) and returns it to where it was. Past the point of no
// return it fails with CodeConflict carrying the server's reason. Not retried.
func (s *SandboxesService) CancelTransition(ctx context.Context, id string) (TransitionCancelOutcome, error) {
	s.client.forget(id)
	var out struct {
		Outcome TransitionCancelOutcome `json:"outcome"`
	}
	if err := s.client.api(ctx, "POST", "/sandbox/"+url.PathEscape(id)+"/transition/cancel", nil, &out, false, nil); err != nil {
		return "", err
	}
	switch out.Outcome {
	case TransitionCleared, TransitionCancelling, TransitionNone:
		return out.Outcome, nil
	default:
		return "", newError(CodeUnavailable, "unexpected response for POST /sandbox/:id/transition/cancel")
	}
}

// DeleteOptions configures Delete.
type DeleteOptions struct {
	// Archive keeps a recoverable copy for 30 days. Nil = the server default (true).
	Archive *bool
}

// Delete deletes a box. A recoverable copy is kept for 30 days unless
// opts.Archive is false.
func (s *SandboxesService) Delete(ctx context.Context, id string, opts DeleteOptions) error {
	s.client.forget(id)
	var body any
	if opts.Archive != nil {
		body = map[string]bool{"archiveBeforeDestroy": *opts.Archive}
	}
	return s.client.api(ctx, "DELETE", "/sandbox/"+url.PathEscape(id), body, nil, false, nil)
}

// SetTier moves a box to another tier. A running box restarts into it.
func (s *SandboxesService) SetTier(ctx context.Context, id, tier string) error {
	s.client.forget(id)
	return s.client.api(ctx, "POST", "/sandbox/"+url.PathEscape(id)+"/tier", map[string]string{"tier": tier}, nil, false, nil)
}

// AccountService reads account state. Reach it as Client.Account.
type AccountService struct {
	client *Client
}

// APIKey is an account API key as listed in Settings (never the secret).
type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Last4      string     `json:"last4"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

// WhoAmI is the identity behind the client's credential.
type WhoAmI struct {
	UserID string  `json:"userId"`
	APIKey *APIKey `json:"apiKey,omitempty"`
}

// WhoAmI returns the account and key the client authenticates as.
func (a *AccountService) WhoAmI(ctx context.Context) (*WhoAmI, error) {
	var out WhoAmI
	if err := a.client.api(ctx, "GET", "/account/whoami", nil, &out, true, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Balance returns the credit balance and auto top-up state, as the dashboard
// reads them. The shape is not part of the stable contract.
func (a *AccountService) Balance(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := a.client.api(ctx, "GET", "/billing/summary", nil, &out, true, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// Usage returns recent metered usage, as the dashboard reads it.
func (a *AccountService) Usage(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := a.client.api(ctx, "GET", "/billing/usage", nil, &out, true, nil); err != nil {
		return nil, err
	}
	return out, nil
}

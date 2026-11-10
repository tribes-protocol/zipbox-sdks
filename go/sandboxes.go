package zipbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"
)

// Status is a box's lifecycle state.
type Status string

// Box lifecycle states. StatusPausing reads as paused: the VM is stopped and its
// disk is safe on its host; its backup is still uploading; Start boots it at once.
const (
	StatusProvisioning Status = "provisioning"
	StatusRunning      Status = "running"
	StatusStopping     Status = "stopping"
	StatusPausing      Status = "pausing" // paused while its backup uploads
	StatusStopped      Status = "stopped"
	StatusArchived     Status = "archived"
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
	ActionStart            Action = "start"
	ActionStop             Action = "stop"
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
	// IsArchived is true when the disk is archived (a stopped box) or an archive is in flight.
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

// Create creates a box and waits until it is ready, re-reading it with
// backoff. The wait has no deadline of its own; bound it with ctx. The request
// carries an Idempotency-Key, so a retried create never makes two boxes. With
// Private set it waits until the box is running with its key instead.
func (s *SandboxesService) Create(ctx context.Context, opts CreateOptions) (*Box, error) {
	var created createResponse
	headers := map[string]string{"Idempotency-Key": newID()}
	body := createBody{Name: opts.Name, Tier: opts.Tier, SSHPublicKey: opts.SSHPublicKey, Mailbox: opts.Mailbox, Private: opts.Private}
	if err := s.client.api(ctx, "POST", "/sandbox/identity/claim", body, &created, true, headers); err != nil {
		return nil, err
	}
	return s.WaitReady(ctx, created.SandboxID)
}

// readiness is the box's Readiness as the server decided it. A control plane that
// predates the field gets the rule before it: ready, private and waiting for a
// tailnet, ended (destroyed, stopped, deleting), or still settling. The fallback can
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
	case StatusDestroyed, StatusStopped:
		return ReadinessGone
	case StatusDeleting:
		return ReadinessDeleting
	}
	return ReadinessSettling
}

// pausedRemedy is how to start a paused box, unless the server says it cannot start now.
func pausedRemedy(d *SandboxDetail) string {
	if d.AllowedActions == nil {
		return "start it with Start"
	}
	for _, action := range d.AllowedActions {
		if action == ActionStart {
			return "start it with Start"
		}
	}
	return "it cannot be started now; see its AllowedActions"
}

// endedError is the error a wait returns for a box that will not become ready by
// itself, or nil while it may. Each is an answered verdict, never retryable.
func endedError(id string, d *SandboxDetail) error {
	var zerr *Error
	switch d.readiness() {
	case ReadinessPaused:
		zerr = newError(CodeBoxNotRunning, "box "+id+" is paused and nothing will start it; "+pausedRemedy(d))
	case ReadinessStartFailed:
		remedy := "start it again with Start"
		if d.Status == StatusStopped {
			remedy = "its name was released; create it again"
		}
		zerr = newError(CodeStartFailed, "box "+id+" failed to start; "+remedy)
	case ReadinessDeleting, ReadinessGone:
		zerr = newError(CodeConflict, "box "+id+" is "+string(d.Status)+" and will not become ready")
	default:
		return nil
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
	zerr := newError(CodeBoxNotRunning, "box "+id+" is "+string(d.Status)+", not ready; start it or wait")
	if d.Readiness == stated {
		zerr.Retryable = stated == ReadinessSettling
	} else {
		// Without readiness (an older control plane), the rule before it: booting or
		// restarting comes back by itself, and nothing else does.
		zerr.Retryable = d.Status == StatusProvisioning || d.Status == StatusRunning
	}
	return zerr
}

// WaitReady re-reads a box with backoff until it is ready, and returns its
// handle. The wait has no deadline of its own; bound it with ctx. A private box
// that has not joined a tailnet yet is returned once it is running with its key.
// A box that will not become ready by itself is an error: paused
// CodeBoxNotRunning, a failed start CodeStartFailed, deleting or gone
// CodeConflict, none of them retryable.
func (s *SandboxesService) WaitReady(ctx context.Context, id string) (*Box, error) {
	detail, err := s.waitSettled(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.readyBox(id, detail), nil
}

// waitSettled re-reads a box with backoff until it is ready or awaits its tailnet,
// and returns that last read. The server's readiness decides: settling keeps
// reading, and a box that will not become ready by itself is endedError. A read
// that fails with a retryable error goes on the same ladder, never sooner than its
// RetryAfter. The wait has no deadline of its own; ctx bounds it.
func (s *SandboxesService) waitSettled(ctx context.Context, id string) (*SandboxDetail, error) {
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
			if ended := endedError(id, detail); ended != nil {
				return nil, ended
			}
			if detail.readiness() != ReadinessSettling {
				return detail, nil
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

// readyBox is the handle for a box waitSettled returned.
func (s *SandboxesService) readyBox(id string, detail *SandboxDetail) *Box {
	box := s.client.Sandbox(id)
	s.client.storeCreds(id, detail)
	box.Detail = detail
	return box
}

// GetOrCreate returns the live box with this name, starting it if it is
// stopped, or creates it with opts. It waits until the box is ready; a private
// box that has not joined a tailnet yet is returned once it is running with its
// key. opts.Name must be empty or name (CodeInvalidRequest otherwise).
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
		if sb.Name == nil || *sb.Name != name || sb.Status == StatusDestroyed || sb.Status == StatusStopped || sb.Status == StatusDeleting {
			continue
		}
		// StatusStopping: a stop is still landing; Start waits it out, then starts.
		// StatusPausing: paused while its backup uploads; Start boots it at once.
		if sb.Status == StatusArchived || sb.Status == StatusStopping || sb.Status == StatusPausing {
			if err := s.Start(ctx, sb.ID); err != nil {
				// The name was being deleted: it is gone now, so make it.
				if IsCode(err, CodeNotFound) {
					break
				}
				// Another caller started it first: the server's idempotent answer to a
				// second restore. A box now coming up is waited on like any other.
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

// Start starts a stopped box. Credit-gated: insufficient_credits when dry.
//
// A stop is accepted at once but its disk takes seconds to minutes to land;
// meanwhile the box reads StatusStopping and the restore answers conflict. Start
// waits that out with backoff and starts the box once it reads StatusArchived.
// A box that comes out of the wait already starting or running (a resize
// restores itself, a failed archive leaves it running) is started. Any other
// conflict is returned as it came.
func (s *SandboxesService) Start(ctx context.Context, id string) error {
	s.client.forget(id)
	waited := false
	for {
		err := s.client.api(ctx, "POST", "/sandbox/"+url.PathEscape(id)+"/restore", nil, nil, false, nil)
		var zerr *Error
		if !errors.As(err, &zerr) || zerr.Code != CodeConflict {
			return err
		}
		detail, getErr := s.getInWait(ctx, id)
		if getErr != nil {
			return getErr
		}
		if detail.Status != StatusStopping {
			// A second conflict after a stop was waited out: the box restored itself,
			// or it is archived and will not restore (that answer is final).
			if waited && (detail.Status == StatusProvisioning || detail.Status == StatusRunning) {
				return nil
			}
			return err
		}
		for attempt := 0; detail.Status == StatusStopping; attempt++ {
			if sleepErr := sleep(ctx, s.client.poll(attempt)); sleepErr != nil {
				return sleepErr
			}
			if detail, getErr = s.getInWait(ctx, id); getErr != nil {
				return getErr
			}
		}
		waited = true
		switch detail.Status {
		case StatusArchived:
		case StatusProvisioning, StatusRunning:
			return nil
		default:
			return err
		}
	}
}

// Stop stops a box: its disk is archived and nothing is billed while stopped.
func (s *SandboxesService) Stop(ctx context.Context, id string) error {
	s.client.forget(id)
	return s.client.api(ctx, "POST", "/sandbox/"+url.PathEscape(id)+"/pause", nil, nil, false, nil)
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

// CancelTransition cancels the box's state change in flight (a stop, a start, a
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

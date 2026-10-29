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

// Box lifecycle states.
const (
	StatusProvisioning Status = "provisioning"
	StatusRunning      Status = "running"
	StatusStopping     Status = "stopping"
	StatusStopped      Status = "stopped"
	StatusArchived     Status = "archived"
	StatusDeleting     Status = "deleting" // signing out of its tailnet before it is destroyed
	StatusDestroyed    Status = "destroyed"
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
	AgentPublicKey *string   `json:"agentPublicKey,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// SandboxDetail is one box as GET /sandbox/{id} returns it.
type SandboxDetail struct {
	Sandbox
	// Ready is true once the box is running and serving its owner.
	Ready bool `json:"ready"`
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

// awaitsTailnet reports a private box that is running with its key but has not
// joined a tailnet: it is never ready until it joins, but the caller can seal an
// auth key to it now.
func awaitsTailnet(d *SandboxDetail) bool {
	joined := d.Tailscale != nil && d.Tailscale.IPv4 != nil && *d.Tailscale.IPv4 != ""
	return d.PrivateMode && !joined && d.Status == StatusRunning && d.AgentPublicKey != nil && *d.AgentPublicKey != ""
}

// WaitReady re-reads a box with backoff until it is ready, and returns its
// handle. The wait has no deadline of its own; bound it with ctx. A private box
// that has not joined a tailnet yet is returned once it is running with its key.
func (s *SandboxesService) WaitReady(ctx context.Context, id string) (*Box, error) {
	for attempt := 0; ; attempt++ {
		detail, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if detail.Ready || awaitsTailnet(detail) {
			box := s.client.Sandbox(id)
			s.client.storeCreds(id, detail)
			box.Detail = detail
			return box, nil
		}
		switch detail.Status {
		case StatusDestroyed, StatusStopped, StatusDeleting:
			return nil, newError(CodeConflict, "box "+id+" is "+string(detail.Status)+" and will not become ready")
		}
		if err := sleep(ctx, jitter(s.client.ladder(attempt, s.client.pollMin, s.client.pollMax), s.client)); err != nil {
			return nil, err
		}
	}
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
		if sb.Status == StatusArchived || sb.Status == StatusStopping {
			if err := s.Start(ctx, sb.ID); err != nil {
				return nil, err
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
	waited := false
	for {
		err := s.client.api(ctx, "POST", "/sandbox/"+url.PathEscape(id)+"/restore", nil, nil, false, nil)
		var zerr *Error
		if !errors.As(err, &zerr) || zerr.Code != CodeConflict {
			return err
		}
		detail, getErr := s.Get(ctx, id)
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
			if sleepErr := sleep(ctx, jitter(s.client.ladder(attempt, s.client.pollMin, s.client.pollMax), s.client)); sleepErr != nil {
				return sleepErr
			}
			if detail, getErr = s.Get(ctx, id); getErr != nil {
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
	return s.client.api(ctx, "POST", "/sandbox/"+url.PathEscape(id)+"/reset", nil, nil, false, nil)
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

package zipbox

import (
	"context"
	"encoding/json"
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
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// SandboxDetail is one box as GET /sandbox/{id} returns it.
type SandboxDetail struct {
	Sandbox
	// Ready is true once the box is running and serving its owner.
	Ready bool `json:"ready"`
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

// CreateOptions configures a new box.
type CreateOptions struct {
	// Name is the box's permanent subdomain. Empty = the account's default box.
	Name string
	// Tier is the machine size (xs, s, m, l, xl, xxl). Empty = the default tier.
	Tier string
	// SSHPublicKey is an OpenSSH public key line authorized on the box.
	SSHPublicKey string
}

// SandboxesService manages boxes. Reach it as Client.Sandboxes.
type SandboxesService struct {
	client *Client
}

type createBody struct {
	Name         string `json:"name,omitempty"`
	Tier         string `json:"tier,omitempty"`
	SSHPublicKey string `json:"sshPublicKey,omitempty"`
}

type createResponse struct {
	SandboxID string `json:"sandboxId"`
	Created   bool   `json:"created"`
}

// Create creates a box and waits until it is ready, re-reading it with
// backoff. The wait has no deadline of its own; bound it with ctx. The request
// carries an Idempotency-Key, so a retried create never makes two boxes.
func (s *SandboxesService) Create(ctx context.Context, opts CreateOptions) (*Box, error) {
	var created createResponse
	headers := map[string]string{"Idempotency-Key": newID()}
	body := createBody{Name: opts.Name, Tier: opts.Tier, SSHPublicKey: opts.SSHPublicKey}
	if err := s.client.api(ctx, "POST", "/sandbox/identity/claim", body, &created, true, headers); err != nil {
		return nil, err
	}
	return s.WaitReady(ctx, created.SandboxID)
}

// WaitReady re-reads a box with backoff until it is ready, and returns its handle.
func (s *SandboxesService) WaitReady(ctx context.Context, id string) (*Box, error) {
	for attempt := 0; ; attempt++ {
		detail, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if detail.Ready {
			box := s.client.Sandbox(id)
			s.client.storeCreds(id, detail)
			box.Detail = detail
			return box, nil
		}
		switch detail.Status {
		case StatusDestroyed, StatusStopped:
			return nil, newError(CodeConflict, "box "+id+" is "+string(detail.Status)+" and will not become ready")
		}
		if err := sleep(ctx, jitter(s.client.ladder(attempt, s.client.pollMin, s.client.pollMax), s.client)); err != nil {
			return nil, err
		}
	}
}

// GetOrCreate returns the live box with this name, starting it if it is
// stopped, or creates it. It waits until the box is ready.
func (s *SandboxesService) GetOrCreate(ctx context.Context, name string) (*Box, error) {
	boxes, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, sb := range boxes {
		if sb.Name == nil || *sb.Name != name || sb.Status == StatusDestroyed || sb.Status == StatusStopped {
			continue
		}
		if sb.Status == StatusArchived || (sb.IsArchived != nil && *sb.IsArchived) {
			if err := s.Start(ctx, sb.ID); err != nil {
				return nil, err
			}
		}
		return s.WaitReady(ctx, sb.ID)
	}
	return s.Create(ctx, CreateOptions{Name: name})
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
func (s *SandboxesService) Start(ctx context.Context, id string) error {
	return s.client.api(ctx, "POST", "/sandbox/"+url.PathEscape(id)+"/restore", nil, nil, false, nil)
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

// Delete deletes a box, keeping a recoverable copy for 30 days.
func (s *SandboxesService) Delete(ctx context.Context, id string) error {
	s.client.forget(id)
	return s.client.api(ctx, "DELETE", "/sandbox/"+url.PathEscape(id), nil, nil, false, nil)
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

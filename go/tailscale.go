package zipbox

import (
	"context"
	"net/url"
	"time"
)

// TailscaleState is a box's tailnet state as the box reports it.
type TailscaleState string

// Tailnet states.
const (
	// TailscaleJoining: the auth key was accepted and the node is coming up.
	TailscaleJoining TailscaleState = "joining"
	// TailscaleConnected: the box is on the tailnet.
	TailscaleConnected TailscaleState = "connected"
	// TailscaleNeedsApproval: a tailnet admin must approve the device.
	TailscaleNeedsApproval TailscaleState = "needsApproval"
	// TailscaleNeedsLogin: the node key expired or the auth key was refused. Connect again with a new key.
	TailscaleNeedsLogin TailscaleState = "needsLogin"
	// TailscaleStopped: logged in but turned off.
	TailscaleStopped TailscaleState = "stopped"
	// TailscaleFailed: the box's Tailscale daemon does not answer.
	TailscaleFailed TailscaleState = "failed"
)

// TailscaleStatus is a box's tailnet status: the live state merged with the
// last known identity.
type TailscaleStatus struct {
	// State is nil when only the last known identity is known (the box is stopped).
	State *TailscaleState `json:"state,omitempty"`
	// Tailnet is the tailnet's name, e.g. "example.com".
	Tailnet *string `json:"tailnet,omitempty"`
	// IPv4 and IPv6 are the box's tailnet addresses.
	IPv4 *string `json:"ipv4,omitempty"`
	IPv6 *string `json:"ipv6,omitempty"`
	// DNSName is the box's MagicDNS name.
	DNSName *string `json:"dnsName,omitempty"`
	// KeyExpiry is when the node key expires; nil when key expiry is off.
	KeyExpiry *time.Time `json:"keyExpiry,omitempty"`
	// Pending is true while an auth key is on its way to the box. Pending going
	// false with no state change means the key never arrived: connect again.
	Pending bool `json:"pending"`
	// Peers are your other devices on the tailnet (online first, at most 50); nil
	// while the box is not reporting.
	Peers []TailscalePeer `json:"peers,omitempty"`
}

// TailscalePeer is one of your other devices on the tailnet, as the box sees it.
type TailscalePeer struct {
	// Name is the device's own name, e.g. "lucys-macbook".
	Name string `json:"name"`
	// DNSName is its MagicDNS name.
	DNSName *string `json:"dnsName,omitempty"`
	// OS is "macOS", "linux", "iOS", "windows", "android", …
	OS *string `json:"os,omitempty"`
	// Online is true while the device is connected to Tailscale.
	Online bool `json:"online"`
	// IPv4 is its tailnet IPv4 (100.x).
	IPv4 *string `json:"ipv4,omitempty"`
	// Connection is "direct" (peer to peer), "relay" (through a Tailscale DERP
	// relay), or nil (no recent traffic). Kept as a string so a later value never
	// breaks decoding.
	Connection *string `json:"connection,omitempty"`
}

// TailscaleService joins a box to your own tailnet. Reach it as Box.Tailscale.
//
// The auth key is sealed on this machine to the box's own key and only the box
// can open it: Zipbox relays it once and never stores or logs it.
type TailscaleService struct {
	box *Box
}

type tailscaleStatusResponse struct {
	Tailscale *TailscaleStatus `json:"tailscale"`
}

type tailscaleConnectBody struct {
	Envelope       *boxEnvelope `json:"envelope"`
	AgentPublicKey string       `json:"agentPublicKey"`
}

func (t *TailscaleService) path() string {
	return "/sandbox/" + url.PathEscape(t.box.ID) + "/tailscale"
}

// Status returns the box's tailnet status, or nil when it never joined one and
// no key is pending.
func (t *TailscaleService) Status(ctx context.Context) (*TailscaleStatus, error) {
	var out tailscaleStatusResponse
	if err := t.box.client.api(ctx, "GET", t.path(), nil, &out, true, nil); err != nil {
		return nil, err
	}
	return out.Tailscale, nil
}

// Connect joins the box to the tailnet the auth key belongs to. Use a one-off,
// pre-approved key. The box must be running. Connect returns once the key is on
// its way; read Status for the join itself.
//
// The box key is read right before sealing, because claiming a box re-mints it.
// If the key changed between the read and the send (box_key_changed), Connect
// reads it again and re-seals once.
func (t *TailscaleService) Connect(ctx context.Context, authKey string) error {
	if authKey == "" {
		return newError(CodeInvalidRequest, "the Tailscale auth key is empty")
	}
	for attempt := 0; ; attempt++ {
		detail, err := t.box.Refresh(ctx)
		if err != nil {
			return err
		}
		if (detail.Status != StatusRunning && detail.Status != StatusProvisioning) || detail.AgentPublicKey == nil || *detail.AgentPublicKey == "" {
			zerr := newError(CodeBoxNotRunning, "box "+t.box.ID+" is "+string(detail.Status)+" and has no key to seal to yet; start it and wait until it is running")
			// A box that is settling grows its key by itself; one that is paused or gone does not.
			zerr.Retryable = detail.readiness() == ReadinessSettling
			return zerr
		}
		envelope, err := sealBoxEnvelope(*detail.AgentPublicKey, tailscaleEnvelopeInfo(t.box.ID), []byte(authKey))
		if err != nil {
			return err
		}
		body := tailscaleConnectBody{Envelope: envelope, AgentPublicKey: *detail.AgentPublicKey}
		err = t.box.client.api(ctx, "PUT", t.path(), body, nil, false, nil)
		if err == nil {
			return nil
		}
		if attempt == 0 && IsCode(err, CodeBoxKeyChanged) {
			continue
		}
		return err
	}
}

// Disconnect leaves the tailnet: the box signs out and its Tailscale state is
// wiped. A private box is made public first, in the same call, so it is never
// left private with no way in.
func (t *TailscaleService) Disconnect(ctx context.Context) error {
	return t.box.client.api(ctx, "DELETE", t.path(), nil, nil, false, nil)
}

// SetPrivate turns private mode ("tailnet only") on or off. A private box
// closes every public way in: its URLs, previews, terminal, IDE, desktop and SSH
// answer only devices on your tailnet. Turning it on needs the box connected to
// a tailnet (tailnet_not_connected otherwise).
func (b *Box) SetPrivate(ctx context.Context, enabled bool) error {
	b.client.forget(b.ID)
	return b.client.api(ctx, "PUT", "/sandbox/"+url.PathEscape(b.ID)+"/private", map[string]bool{"enabled": enabled}, nil, false, nil)
}

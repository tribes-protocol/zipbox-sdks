package zipbox

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ForkSourcePolicy is how a fork may treat its source box.
type ForkSourcePolicy string

const (
	// ForkRequirePaused refuses a running source (CodePauseRequired). A paused
	// source stays paused.
	ForkRequirePaused ForkSourcePolicy = "require_paused"
	// ForkPauseAndResume lets a running source pause for its capture: its sessions
	// disconnect and it resumes right after. A paused source stays paused.
	ForkPauseAndResume ForkSourcePolicy = "pause_and_resume"
)

// ForkPhase is where a fork operation stands. Kept as a string, so a later phase
// never breaks decoding.
type ForkPhase string

// Fork phases.
const (
	// ForkPreparingIdentity: the new box's name and mailbox address are being reserved.
	ForkPreparingIdentity ForkPhase = "preparing_identity"
	// ForkQueued: the new box exists; the capture waits its turn on the source.
	ForkQueued ForkPhase = "queued"
	// ForkCapturing: the source's disk is being captured.
	ForkCapturing ForkPhase = "capturing"
	// ForkCopying: the allowed files are being copied into the new box's fresh filesystem.
	ForkCopying ForkPhase = "copying"
	// ForkStarting: the new box boots.
	ForkStarting ForkPhase = "starting"
	// ForkComplete: the new box has its own disk and serves.
	ForkComplete ForkPhase = "complete"
	// ForkCancelling: a cancel is on its way.
	ForkCancelling ForkPhase = "cancelling"
	// ForkCancelled: the fork was cancelled; the new box was discarded, its name kept.
	ForkCancelled ForkPhase = "cancelled"
	// ForkFailed: the fork ended without a box; SandboxFork.Error says why.
	ForkFailed ForkPhase = "failed"
)

// ForkSourceRecovery is where a fork's source stands.
type ForkSourceRecovery string

// Source recovery states.
const (
	// ForkSourceNotNeeded: the source was paused and stays paused.
	ForkSourceNotNeeded ForkSourceRecovery = "not_needed"
	// ForkSourcePending: the fork paused the source and owes its resume.
	ForkSourcePending ForkSourceRecovery = "pending"
	// ForkSourceResumed: the source runs again.
	ForkSourceResumed ForkSourceRecovery = "resumed"
	// ForkSourceSuperseded: a newer owner action (a pause, a delete) replaced the resume.
	ForkSourceSuperseded ForkSourceRecovery = "superseded"
)

// ForkError is why a fork ended without a box. Code is one of CodeSourceUnsupported,
// CodeSourceGone, CodeIdentityReused, CodeCopyFailed or CodeStartFailed.
type ForkError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// SandboxFork is one fork operation as GET /sandbox/{childId}/fork returns it,
// and as a box being forked into carries it on its row.
type SandboxFork struct {
	OperationID     string             `json:"operationId"`
	SourceSandboxID string             `json:"sourceSandboxId"`
	ChildSandboxID  string             `json:"childSandboxId"`
	SourcePolicy    ForkSourcePolicy   `json:"sourcePolicy"`
	Phase           ForkPhase          `json:"phase"`
	SourceRecovery  ForkSourceRecovery `json:"sourceRecovery"`
	// CaptureMode is "reflink" (copy-on-write capture) or "copy"; nil until captured.
	CaptureMode *string `json:"captureMode,omitempty"`
	// WaitingOn says why the current step waits (software_install, legacy_chisel,
	// busy, host_offline, capacity); nil while it moves.
	WaitingOn *string `json:"waitingOn,omitempty"`
	// Attempt is the retry ladder's position for the current step.
	Attempt           int        `json:"attempt"`
	NextAttemptAt     *time.Time `json:"nextAttemptAt,omitempty"`
	CancelRequestedAt *time.Time `json:"cancelRequestedAt,omitempty"`
	// Error is set when Phase is ForkFailed.
	Error       *ForkError `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

// ForkOptions configures Fork. Name and SourcePolicy are required.
//
// Printing ForkOptions (any fmt verb) or marshalling it never shows
// TailscaleAuthKey.
type ForkOptions struct {
	// Name is the new box's permanent subdomain. It is kept even if the fork is cancelled.
	Name string
	// SourcePolicy is ForkRequirePaused or ForkPauseAndResume.
	SourcePolicy ForkSourcePolicy
	// Private starts the new box in private mode ("tailnet only"): never public,
	// reachable only from your tailnet once it joins. False (the default) starts
	// it public, whatever the source's privacy.
	Private bool
	// TailscaleAuthKey, when set, joins the new box to your tailnet once it runs:
	// it is sealed on this machine to the new box's own key and never sent to
	// /fork. Needs the wait (NoWait false).
	TailscaleAuthKey string `json:"-"`
	// NoWait returns the new box as soon as the fork is accepted, without waiting
	// until it is ready. False (the default) waits.
	NoWait bool
}

func (o ForkOptions) redactedKey() string {
	if o.TailscaleAuthKey == "" {
		return ""
	}
	return "[redacted]"
}

// String prints the options with the Tailscale auth key redacted.
func (o ForkOptions) String() string {
	return fmt.Sprintf("{Name:%s SourcePolicy:%s Private:%t TailscaleAuthKey:%s NoWait:%t}", o.Name, o.SourcePolicy, o.Private, o.redactedKey(), o.NoWait)
}

// GoString is %#v, with the Tailscale auth key redacted.
func (o ForkOptions) GoString() string {
	return fmt.Sprintf("zipbox.ForkOptions{Name:%q, SourcePolicy:%q, Private:%t, TailscaleAuthKey:%q, NoWait:%t}", o.Name, string(o.SourcePolicy), o.Private, o.redactedKey(), o.NoWait)
}

// Format makes every fmt verb print the redacted form, so no verb can reach the key.
func (o ForkOptions) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		_, _ = io.WriteString(f, o.GoString())
		return
	}
	if verb == 'q' {
		_, _ = io.WriteString(f, strconv.Quote(o.String()))
		return
	}
	_, _ = io.WriteString(f, o.String())
}

type forkBody struct {
	Name         string           `json:"name"`
	SourcePolicy ForkSourcePolicy `json:"sourcePolicy"`
	Private      bool             `json:"private,omitempty"`
}

type forkResponse struct {
	OperationID string `json:"operationId"`
	// Nil once a cancelled or failed fork removed the box (a replay of the same
	// Idempotency-Key answers its tombstone).
	Sandbox *Sandbox    `json:"sandbox"`
	Fork    SandboxFork `json:"fork"`
}

// forkEndedError is the error a fork that ended without its box answers: the
// fork's own code (an answered verdict, never retried), or CodeConflict for a
// cancel. Nil while the fork has not ended that way.
func forkEndedError(fork *SandboxFork) error {
	if fork.Error != nil {
		// A code this SDK does not know yet is a conflict, with the server's message,
		// as in every SDK.
		code := fork.Error.Code
		if !knownCode(code) {
			code = CodeConflict
		}
		zerr := newError(code, fork.Error.Message)
		zerr.Retryable = false
		return zerr
	}
	if fork.Phase == "cancelled" {
		return newError(CodeConflict, "the fork making box "+fork.ChildSandboxID+" was cancelled")
	}
	return nil
}

// Fork makes a new box from a consistent capture of the source's disk: same
// files, settings and size, a new name and a fresh identity. RAM and running
// processes are not copied. The new box starts public unless opts.Private.
//
// The request carries one Idempotency-Key reused on every retry, so a retried
// fork never makes two boxes. By default Fork then waits until the new box is
// ready (a private one: running with its key), with backoff and no deadline of
// its own; bound it with ctx. A ctx that ends stops only this wait: the fork
// keeps running (cancel it with CancelTransition on the new box). A new box its
// fork removed returns the fork's own error code (CodeCopyFailed, …), or
// CodeConflict when it was cancelled. With TailscaleAuthKey the new box then
// joins your tailnet, as Box.Tailscale.Connect does. With NoWait the new box is
// returned as accepted: Detail is its row with Ready false and Fork set.
func (s *SandboxesService) Fork(ctx context.Context, sourceID string, opts ForkOptions) (*Box, error) {
	if opts.TailscaleAuthKey != "" {
		if strings.TrimSpace(opts.TailscaleAuthKey) == "" {
			return nil, newError(CodeInvalidRequest, "the Tailscale auth key is empty")
		}
		if opts.NoWait {
			return nil, newError(CodeInvalidRequest, "TailscaleAuthKey needs the wait: the key is sealed to the new box once it is running; drop NoWait, or connect it later with Box.Tailscale.Connect")
		}
	}
	// A running source pauses for its capture: its cached address goes with it.
	s.client.forget(sourceID)
	var accepted forkResponse
	headers := map[string]string{"Idempotency-Key": newID()}
	body := forkBody{Name: opts.Name, SourcePolicy: opts.SourcePolicy, Private: opts.Private}
	if err := s.client.api(ctx, "POST", "/sandbox/"+url.PathEscape(sourceID)+"/fork", body, &accepted, true, headers); err != nil {
		return nil, err
	}
	childID := accepted.Fork.ChildSandboxID
	if childID == "" || accepted.OperationID == "" {
		return nil, newError(CodeUnavailable, "unexpected response for POST /sandbox/:id/fork")
	}
	if accepted.Sandbox == nil {
		// The key found a fork that already ended and removed its box: that fork's
		// verdict, never an "unexpected response".
		if err := forkEndedError(&accepted.Fork); err != nil {
			return nil, err
		}
		return nil, newError(CodeConflict, "the fork making box "+childID+" ended")
	}
	if opts.NoWait {
		row := *accepted.Sandbox
		fork := accepted.Fork
		row.Fork = &fork
		box := s.client.Sandbox(childID)
		box.Detail = &SandboxDetail{Sandbox: row}
		return box, nil
	}
	detail, err := s.waitSettled(ctx, childID)
	if err != nil {
		// A cancelled or failed fork removes its box: the box reads not_found while
		// the fork still says why. A fork that could not be read is no verdict.
		if IsCode(err, CodeNotFound) {
			fork, forkErr := s.GetFork(ctx, childID)
			if zerr, ok := asError(forkErr); ok && zerr.Retryable {
				return nil, forkErr
			}
			if forkErr == nil {
				if ended := forkEndedError(fork); ended != nil {
					return nil, ended
				}
			}
		}
		return nil, err
	}
	box := s.readyBox(childID, detail)
	if opts.TailscaleAuthKey != "" {
		if err := box.Tailscale.Connect(ctx, strings.TrimSpace(opts.TailscaleAuthKey)); err != nil {
			return nil, err
		}
	}
	return box, nil
}

// GetFork returns the fork that is making (or made) the box childID: its phase,
// source recovery, waits and error. CodeNotFound when no fork made this box.
func (s *SandboxesService) GetFork(ctx context.Context, childID string) (*SandboxFork, error) {
	var out SandboxFork
	if err := s.client.api(ctx, "GET", "/sandbox/"+url.PathEscape(childID)+"/fork", nil, &out, true, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Fork makes a new box from this one (see SandboxesService.Fork) and returns the
// new box.
func (b *Box) Fork(ctx context.Context, opts ForkOptions) (*Box, error) {
	return b.client.Sandboxes.Fork(ctx, b.ID, opts)
}

// ForkStatus returns the fork that is making (or made) this box (see
// SandboxesService.GetFork).
func (b *Box) ForkStatus(ctx context.Context) (*SandboxFork, error) {
	return b.client.Sandboxes.GetFork(ctx, b.ID)
}

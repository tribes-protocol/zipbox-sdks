package zipbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// PreviewService publishes ports on a box. Reach it as Box.Preview.
type PreviewService struct {
	box *Box
}

// Preview is a published port.
type Preview struct {
	// URL is the HTTPS address, e.g. https://web.demo.zbox.sh.
	URL string `json:"url"`
	// Label is the subdomain label.
	Label string `json:"label"`
	// Host is the full hostname.
	Host string `json:"host"`
}

// Create publishes 127.0.0.1:port on the box at https://<label>.<box host>.
// An empty label is "p<port>". On a public box anyone with the URL can reach
// the port; on a private box, only devices on the owner's tailnet.
func (s *PreviewService) Create(ctx context.Context, port int, label string) (*Preview, error) {
	if label == "" {
		label = "p" + strconv.Itoa(port)
	}
	host, err := s.box.host(ctx)
	if err != nil {
		return nil, err
	}
	fqdn := label + "." + host
	for attempt := 0; ; attempt++ {
		retryAfter := time.Duration(0)
		res, err := s.box.ExecArgv(ctx, []string{"tribes-caddy", "add", fqdn, "127.0.0.1:" + strconv.Itoa(port), "--no-wait"})
		if err != nil {
			var zerr *Error
			if !errors.As(err, &zerr) || !zerr.Retryable {
				return nil, err
			}
			retryAfter = zerr.RetryAfter
		} else {
			if res.ExitCode == 0 {
				break
			}
			if res.ExitCode != 75 {
				return nil, newError(CodeConflict, "tribes-caddy add failed: "+strings.TrimSpace(res.Stderr+res.Stdout))
			}
			lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
			var pending struct {
				Status       string `json:"status"`
				RetryAfterMs *int64 `json:"retryAfterMs"`
			}
			if json.Unmarshal([]byte(lines[len(lines)-1]), &pending) != nil || pending.Status != "pending" || pending.RetryAfterMs == nil || *pending.RetryAfterMs < 0 {
				return nil, newError(CodeConflict, "preview returned invalid pending status")
			}
			retryAfter = time.Duration(*pending.RetryAfterMs) * time.Millisecond
		}
		if err := sleep(ctx, max(retryAfter, s.box.client.backoff(attempt))); err != nil {
			return nil, err
		}
	}
	return &Preview{URL: "https://" + fqdn, Label: label, Host: fqdn}, nil
}

// Remove unpublishes a preview by its label (or full host).
func (s *PreviewService) Remove(ctx context.Context, label string) error {
	host, err := s.box.host(ctx)
	if err != nil {
		return err
	}
	fqdn := label
	if !strings.HasSuffix(label, "."+host) {
		fqdn = label + "." + host
	}
	res, err := s.box.ExecArgv(ctx, []string{"tribes-caddy", "rm", fqdn})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return newError(CodeConflict, "tribes-caddy rm failed: "+strings.TrimSpace(res.Stderr+res.Stdout))
	}
	return nil
}

// LogOptions filters Logs.
type LogOptions struct {
	// Since is a journalctl time ("1 hour ago", "2026-10-19 10:00"). Empty = all.
	Since string
	// Lines limits output to the last N lines. 0 = no limit.
	Lines int
	// Unit restricts to one systemd unit.
	Unit string
}

// Logs returns the box's system journal (journalctl). Command output is its
// own log: see Commands.Events.
func (b *Box) Logs(ctx context.Context, opts LogOptions) (string, error) {
	argv := []string{"journalctl", "-o", "short-iso", "--no-pager"}
	if opts.Since != "" {
		argv = append(argv, "--since", opts.Since)
	}
	if opts.Lines > 0 {
		argv = append(argv, "-n", strconv.Itoa(opts.Lines))
	}
	if opts.Unit != "" {
		argv = append(argv, "-u", opts.Unit)
	}
	res, err := b.ExecArgv(ctx, argv)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", newError(CodeConflict, "journalctl failed: "+strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// SSHInfo is how to reach a box over SSH (IPv6).
type SSHInfo struct {
	Host    string `json:"host"`
	User    string `json:"user"`
	Command string `json:"command"`
}

// SSHInfo returns the box's SSH address and the command line that reaches it.
// Your public key must be on the box (Settings → SSH keys, or SSHPublicKey at create).
func (b *Box) SSHInfo(ctx context.Context) (*SSHInfo, error) {
	detail, err := b.Refresh(ctx)
	if err != nil {
		return nil, err
	}
	if detail.IPv6 == nil || *detail.IPv6 == "" {
		return nil, newError(CodeBoxNotRunning, "box "+b.ID+" has no address yet; start it first")
	}
	return &SSHInfo{Host: *detail.IPv6, User: "root", Command: "ssh root@" + *detail.IPv6}, nil
}

// desktopDisplay is the X display the box's desktop runs on.
const desktopDisplay = ":10"

// screenshotCommand captures it with ffmpeg's x11grab, which the platform image
// ships (it has no ImageMagick).
const screenshotCommand = "set -o pipefail; ffmpeg -loglevel error -f x11grab -i " + desktopDisplay +
	" -frames:v 1 -f image2pipe -vcodec png - | base64 -w0"

// Screenshot returns a PNG of the box's desktop. The desktop must be running
// (open it from the dashboard or start the browser).
func (b *Box) Screenshot(ctx context.Context) ([]byte, error) {
	res, err := b.Exec(ctx, screenshotCommand)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		return nil, newError(CodeConflict, "no desktop to capture: "+strings.TrimSpace(res.Stderr))
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(res.Stdout))
}

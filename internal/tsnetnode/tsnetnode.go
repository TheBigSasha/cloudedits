// Package tsnetnode wraps tailscale.com/tsnet to run cloudedits as an
// independent userspace Tailscale node with automatic Let's Encrypt HTTPS
// certificates and verified Tailnet peer WhoIs authentication.
package tsnetnode

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"tailscale.com/client/tailscale"
	"tailscale.com/tsnet"
)

// ErrHTTPSNotEnabled indicates MagicDNS or HTTPS Certificates are not yet
// enabled in the Tailscale admin console.
var ErrHTTPSNotEnabled = errors.New("tailscale HTTPS certificates are not enabled; enable MagicDNS and HTTPS at https://login.tailscale.com/admin/dns")

// Options configures a tsnet node.
type Options struct {
	Hostname string
	StateDir string
	AuthKey  string
}

// Node manages a running embedded tsnet.Server.
type Node struct {
	srv         *tsnet.Server
	lc          *tailscale.LocalClient
	mu          sync.RWMutex
	fqdn        string
	tailscaleIP string
	ownerLogin  string
	authURL     string
}

// Start brings up the embedded tsnet node and checks CertDomains preflight.
func Start(ctx context.Context, opts Options) (*Node, error) {
	hostname := strings.TrimSpace(opts.Hostname)
	if hostname == "" {
		hostname = "library"
	}
	if opts.StateDir != "" {
		if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
			return nil, fmt.Errorf("create tsnet state dir: %w", err)
		}
	}

	srv := &tsnet.Server{
		Hostname: hostname,
		Dir:      opts.StateDir,
		AuthKey:  opts.AuthKey,
	}

	status, err := srv.Up(ctx)
	if err != nil {
		_ = srv.Close()
		return nil, fmt.Errorf("tsnet up: %w", err)
	}

	lc, err := srv.LocalClient()
	if err != nil {
		_ = srv.Close()
		return nil, fmt.Errorf("tsnet local client: %w", err)
	}

	n := &Node{
		srv: srv,
		lc:  lc,
	}

	if status != nil {
		n.authURL = status.AuthURL
		for _, ip := range status.TailscaleIPs {
			if ip.Is4() {
				n.tailscaleIP = ip.String()
				break
			}
		}
		if n.tailscaleIP == "" && len(status.TailscaleIPs) > 0 {
			n.tailscaleIP = status.TailscaleIPs[0].String()
		}
		if status.Self != nil {
			if status.Self.DNSName != "" {
				n.fqdn = strings.TrimSuffix(status.Self.DNSName, ".")
			}
			if u, ok := status.User[status.Self.UserID]; ok {
				n.ownerLogin = strings.ToLower(strings.TrimSpace(u.LoginName))
			}
		}
	}

	domains := srv.CertDomains()
	if len(domains) == 0 {
		return n, ErrHTTPSNotEnabled
	}
	if n.fqdn == "" {
		n.fqdn = strings.TrimSuffix(domains[0], ".")
	}
	return n, nil
}

// ListenTLS opens a TLS listener on :443 with automatic Let's Encrypt certs.
func (n *Node) ListenTLS() (net.Listener, error) {
	if n == nil || n.srv == nil {
		return nil, errors.New("tsnet node not started")
	}
	return n.srv.ListenTLS("tcp", ":443")
}

// FQDN returns the node's MagicDNS hostname (e.g. library.tailnet.ts.net).
func (n *Node) FQDN() string {
	if n == nil {
		return ""
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.fqdn
}

// TailscaleIP returns the node's Tailscale IP address (e.g. 100.x.y.z).
func (n *Node) TailscaleIP() string {
	if n == nil {
		return ""
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.tailscaleIP
}

// AuthURL returns an interactive Tailscale login URL when no auth key was supplied.
func (n *Node) AuthURL() string {
	if n == nil {
		return ""
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.authURL
}

// CertDomains returns the node's HTTPS-capable MagicDNS domains.
func (n *Node) CertDomains() []string {
	if n == nil || n.srv == nil {
		return nil
	}
	return n.srv.CertDomains()
}

// WhoIs resolves the Tailnet peer login for remoteAddr and checks whether it
// belongs to the same tailnet owner account.
func (n *Node) WhoIs(ctx context.Context, remoteAddr string) (login string, isOwner bool, err error) {
	if n == nil || n.lc == nil {
		return "", false, errors.New("tsnet local client unavailable")
	}
	who, err := n.lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return "", false, err
	}
	if who == nil || who.UserProfile == nil {
		return "", false, errors.New("missing tailscale user profile")
	}
	login = strings.ToLower(strings.TrimSpace(who.UserProfile.LoginName))
	if login == "" {
		return "", false, errors.New("empty tailscale login name")
	}

	n.mu.RLock()
	owner := n.ownerLogin
	n.mu.RUnlock()

	// If owner login was resolved, require exact same owner account (rejects external shared peers by default).
	// For tagged nodes where ownerLogin is empty, allow same-tailnet verified peers.
	if owner == "" {
		return login, true, nil
	}
	return login, login == owner, nil
}

// Close shuts down the embedded tsnet node.
func (n *Node) Close() error {
	if n == nil || n.srv == nil {
		return nil
	}
	return n.srv.Close()
}

// StateDir returns the tsnet state directory under dataDir.
func StateDir(dataDir string) string {
	return filepath.Join(dataDir, "tsnet-state")
}

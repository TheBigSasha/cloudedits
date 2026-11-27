// Package tsnetnode wraps tailscale.com/tsnet to run cloudedits as an
// independent userspace Tailscale node with automatic Let's Encrypt HTTPS
// certificates and verified Tailnet peer WhoIs authentication.
package tsnetnode

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/client/tailscale"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
	"tailscale.com/types/netmap"
	"tailscale.com/types/views"
)

// taggedDevicesLogin is the shared pseudo-user login Tailscale assigns to
// ACL-tagged nodes. It is never a human owner account.
const taggedDevicesLogin = "tagged-devices"

// ErrHTTPSNotEnabled indicates MagicDNS or HTTPS Certificates are not yet
// enabled in the Tailscale admin console.
var ErrHTTPSNotEnabled = errors.New("tailscale HTTPS certificates are not enabled; enable MagicDNS and HTTPS at https://login.tailscale.com/admin/dns")

// Options configures a tsnet node.
type Options struct {
	Hostname string
	StateDir string
	// AuthKey is used only to enrol a node that has no state yet. It is
	// handed to tsnet in memory and never written anywhere by this package.
	AuthKey string
	// OwnerUserID explicitly pins the Tailscale UserID authorized as the
	// library owner. Required when enrolling with a tagged auth key (tagged
	// nodes have no human owner and fail closed otherwise). When zero on a
	// user-owned node, Start derives the owner from status.Self.UserID.
	OwnerUserID tailcfg.UserID

	// ControlURL, Logf and CertPair exist for tests that run a node against
	// an in-process coordination server, which cannot issue certificates.
	// No command-line flag sets them.
	ControlURL string
	Logf       logger.Logf
	CertPair   func(ctx context.Context, domain string) (certPEM, keyPEM []byte, err error)
}

// Enrolled reports whether stateDir holds a node identity from an earlier
// enrolment, so the node can come back up without an auth key.
func Enrolled(stateDir string) bool {
	info, err := os.Stat(filepath.Join(stateDir, "tailscaled.state"))
	return err == nil && info.Size() > 0
}

// Node manages a running embedded tsnet.Server.
type Node struct {
	srv         *tsnet.Server
	lc          *tailscale.LocalClient
	mu          sync.RWMutex
	fqdn        string
	tailscaleIP string
	ownerUserID tailcfg.UserID
	ownerLogin  string
	isTagged    bool
	authURL     string
	// certPair and whoIs replace LocalAPI calls in unit tests.
	certPair func(ctx context.Context, domain string) (certPEM, keyPEM []byte, err error)
	whoIs    func(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
}

func isTaggedSelf(self *ipnstate.PeerStatus, profile tailcfg.UserProfile, hasProfile bool) bool {
	if self == nil || self.UserID.IsZero() {
		return true
	}
	if self.Tags != nil && self.Tags.Len() > 0 {
		return true
	}
	if hasProfile && strings.EqualFold(strings.TrimSpace(profile.LoginName), taggedDevicesLogin) {
		return true
	}
	return false
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
		Hostname:   hostname,
		Dir:        opts.StateDir,
		AuthKey:    opts.AuthKey,
		ControlURL: opts.ControlURL,
	}
	if opts.Logf != nil {
		srv.Logf = opts.Logf
		srv.UserLogf = opts.Logf
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
		srv:      srv,
		lc:       lc,
		certPair: opts.CertPair,
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
			// A MagicDNS name is fully qualified (host.tailnet.ts.net.);
			// a bare host label is not an address other devices can use.
			if name := strings.TrimSuffix(status.Self.DNSName, "."); strings.Contains(name, ".") {
				n.fqdn = name
			}
			selfUser, hasSelfUser := status.User[status.Self.UserID]
			n.isTagged = isTaggedSelf(status.Self, selfUser, hasSelfUser)
			switch {
			case !opts.OwnerUserID.IsZero():
				n.ownerUserID = opts.OwnerUserID
				if u, ok := status.User[opts.OwnerUserID]; ok {
					n.ownerLogin = strings.ToLower(strings.TrimSpace(u.LoginName))
				}
			case !n.isTagged:
				n.ownerUserID = status.Self.UserID
				if hasSelfUser {
					n.ownerLogin = strings.ToLower(strings.TrimSpace(selfUser.LoginName))
				}
			}
		} else if !opts.OwnerUserID.IsZero() {
			n.ownerUserID = opts.OwnerUserID
		}
	} else if !opts.OwnerUserID.IsZero() {
		n.ownerUserID = opts.OwnerUserID
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

// OwnerUserID returns the pinned Tailscale UserID authorized as the library
// owner, or 0 when no human owner is established (e.g. a tagged node without
// an explicit OwnerUserID configured).
func (n *Node) OwnerUserID() tailcfg.UserID {
	if n == nil {
		return 0
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.ownerUserID
}

// OwnerLogin returns the display login name associated with OwnerUserID, when
// known. It is informational only and never used for authorization decisions.
func (n *Node) OwnerLogin() string {
	if n == nil {
		return ""
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.ownerLogin
}

// IsTagged reports whether this node is enrolled as an ACL-tagged device (or
// lacks a human owner identity).
func (n *Node) IsTagged() bool {
	if n == nil {
		return false
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.isTagged
}

// WhoIs resolves the Tailnet peer identity for remoteAddr and checks whether
// it belongs to the pinned owner UserID. Authorization fails closed:
//   - if no owner UserID is pinned (ownerUserID == 0), every peer is denied;
//   - tagged peers and the shared "tagged-devices" pseudo-user are always denied;
//   - comparison is strictly on tailcfg.UserID, never on LoginName strings.
func (n *Node) WhoIs(ctx context.Context, remoteAddr string) (login string, isOwner bool, err error) {
	if n == nil {
		return "", false, errors.New("tsnet local client unavailable")
	}
	lookup := n.whoIs
	if lookup == nil {
		if n.lc == nil {
			return "", false, errors.New("tsnet local client unavailable")
		}
		lookup = n.lc.WhoIs
	}
	who, err := lookup(ctx, remoteAddr)
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
	peerUserID := who.UserProfile.ID
	if peerUserID.IsZero() {
		return "", false, errors.New("missing tailscale user id")
	}
	if (who.Node != nil && who.Node.IsTagged()) || login == taggedDevicesLogin {
		return login, false, nil
	}

	n.mu.RLock()
	ownerID := n.ownerUserID
	n.mu.RUnlock()

	if ownerID.IsZero() {
		return login, false, nil
	}
	return login, peerUserID == ownerID, nil
}

// CheckMagicDNS asks the node's own tailnet resolver, the one other tailnet
// devices use, for the node's name and reports whether it answers with the
// node's address. On Linux and Windows, tsnet's noop OS DNS manager can prevent
// the in-process resolver from populating host records; when QueryDNS returns
// a non-success RCode or error, CheckMagicDNS falls back to inspecting the
// node's authoritative NetMap via WatchIPNBus.
func (n *Node) CheckMagicDNS(ctx context.Context) error {
	fqdn, ip := n.FQDN(), n.TailscaleIP()
	if fqdn == "" {
		return errors.New("the node has no MagicDNS name; enable MagicDNS at https://login.tailscale.com/admin/dns")
	}
	raw, _, err := n.lc.QueryDNS(ctx, fqdn+".", "A")
	if err != nil {
		return n.checkMagicDNSViaNetMap(ctx, fqdn, ip, fmt.Errorf("query %s: %w", fqdn, err))
	}
	var p dnsmessage.Parser
	hdr, err := p.Start(raw)
	if err != nil {
		return fmt.Errorf("parse answer for %s: %w", fqdn, err)
	}
	if hdr.RCode != dnsmessage.RCodeSuccess {
		origErr := fmt.Errorf("%s does not resolve on the tailnet (%s)", fqdn, hdr.RCode)
		return n.checkMagicDNSViaNetMap(ctx, fqdn, ip, origErr)
	}
	if err := p.SkipAllQuestions(); err != nil {
		return err
	}
	var got []string
	for {
		h, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return err
		}
		if h.Type != dnsmessage.TypeA {
			_ = p.SkipAnswer()
			continue
		}
		a, err := p.AResource()
		if err != nil {
			return err
		}
		addr := net.IP(a.A[:]).String()
		if addr == ip {
			return nil
		}
		got = append(got, addr)
	}
	if len(got) == 0 {
		origErr := fmt.Errorf("%s resolves to %v on the tailnet, not this node (%s)", fqdn, got, ip)
		return n.checkMagicDNSViaNetMap(ctx, fqdn, ip, origErr)
	}
	return fmt.Errorf("%s resolves to %v on the tailnet, not this node (%s)", fqdn, got, ip)
}

func (n *Node) checkMagicDNSViaNetMap(ctx context.Context, fqdn, ip string, origErr error) error {
	if n == nil || n.lc == nil {
		return origErr
	}
	watchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	watcher, err := n.lc.WatchIPNBus(watchCtx, ipn.NotifyInitialNetMap)
	if err != nil {
		return origErr
	}
	defer func() { _ = watcher.Close() }()

	for {
		notify, err := watcher.Next()
		if err != nil {
			return origErr
		}
		if notify.NetMap != nil {
			return checkMagicDNSFromNetMap(notify.NetMap, fqdn, ip, origErr)
		}
	}
}

func normalizeDNSName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// checkMagicDNSFromNetMap evaluates whether fqdn resolves to ip in the
// authoritative Tailscale NetworkMap, mirroring tailscale's local DNS host
// construction (SelfNode, Peers, and ExtraRecords) when MagicDNS is proxied.
func checkMagicDNSFromNetMap(nm *netmap.NetworkMap, fqdn, ip string, origErr error) error {
	if nm == nil {
		if origErr != nil {
			return origErr
		}
		return fmt.Errorf("%s does not resolve on the tailnet", fqdn)
	}
	if !nm.DNS.Proxied {
		return fmt.Errorf("%s does not resolve on the tailnet (MagicDNS disabled)", fqdn)
	}

	target := normalizeDNSName(fqdn)
	var got []string
	seen := make(map[string]bool)
	addIP := func(addr netip.Addr) {
		if !addr.Is4() {
			return
		}
		s := addr.String()
		if !seen[s] {
			seen[s] = true
			got = append(got, s)
		}
	}
	addNode := func(name string, addrs views.Slice[netip.Prefix]) {
		if normalizeDNSName(name) != target {
			return
		}
		for i := range addrs.Len() {
			addIP(addrs.At(i).Addr())
		}
	}

	if nm.SelfNode.Valid() {
		selfName := nm.SelfNode.Name()
		if selfName == "" {
			selfName = nm.Name
		}
		addNode(selfName, nm.SelfNode.Addresses())
	} else if nm.Name != "" {
		addNode(nm.Name, nm.GetAddresses())
	}
	for _, peer := range nm.Peers {
		if !peer.Valid() {
			continue
		}
		addNode(peer.Name(), peer.Addresses())
	}
	for _, rec := range nm.DNS.ExtraRecords {
		if rec.Type != "" && !strings.EqualFold(rec.Type, "A") {
			continue
		}
		if normalizeDNSName(rec.Name) != target {
			continue
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(rec.Value))
		if err != nil {
			continue
		}
		addIP(addr)
	}

	if len(got) == 0 {
		if origErr != nil {
			return origErr
		}
		return fmt.Errorf("%s does not resolve on the tailnet", fqdn)
	}
	for _, addr := range got {
		if addr == ip {
			return nil
		}
	}
	return fmt.Errorf("%s resolves to %v on the tailnet, not this node (%s)", fqdn, got, ip)
}

// CheckHTTPS obtains the node's HTTPS certificate the same way its TLS
// listener does (through the node's LocalAPI, which requests one from Let's
// Encrypt the first time) and verifies it is valid for the node's MagicDNS
// name against roots (nil means the system roots). It returns the
// certificate's expiry. A tsnet node cannot dial its own address, so this
// checks the certificate the listener serves rather than a handshake with it.
func (n *Node) CheckHTTPS(ctx context.Context, roots *x509.CertPool) (time.Time, error) {
	fqdn := n.FQDN()
	if fqdn == "" {
		return time.Time{}, errors.New("the node has no MagicDNS name")
	}
	if len(n.CertDomains()) == 0 {
		return time.Time{}, ErrHTTPSNotEnabled
	}
	fetch := n.certPair
	if fetch == nil {
		fetch = n.lc.CertPair
	}
	certPEM, _, err := fetch(ctx, fqdn)
	if err != nil {
		return time.Time{}, fmt.Errorf("get HTTPS certificate for %s: %w", fqdn, err)
	}
	var chain []*x509.Certificate
	for rest := certPEM; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse certificate for %s: %w", fqdn, err)
		}
		chain = append(chain, c)
	}
	if len(chain) == 0 {
		return time.Time{}, fmt.Errorf("no certificate returned for %s", fqdn)
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{DNSName: fqdn, Roots: roots, Intermediates: inter}); err != nil {
		return time.Time{}, fmt.Errorf("certificate for %s does not verify: %w", fqdn, err)
	}
	return chain[0].NotAfter, nil
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

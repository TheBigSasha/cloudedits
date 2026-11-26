package tsnetnode

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"
)

const goodKey = "tskey-auth-kTESTKEYCNTRL-0123456789abcdef"

// startTestControl runs an in-process coordination server and DERP relay
// that accept exactly one auth key. This test server names nodes by bare
// host label and serves no MagicDNS host records, so the record a real
// tailnet would serve for the first node (always 100.64.0.1 here) is given
// as an extra DNS record; magicDNSAddr overrides it.
func startTestControl(t *testing.T, key string, magicDNSAddr ...string) *testcontrol.Server {
	t.Helper()
	netns.SetEnabled(false)
	t.Cleanup(func() { netns.SetEnabled(true) })
	derpMap := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	control := &testcontrol.Server{
		DERPMap: derpMap,
		DNSConfig: &tailcfg.DNSConfig{Proxied: true, ExtraRecords: []tailcfg.DNSRecord{
			{Name: "library.tail-scale.ts.net", Type: "A", Value: append(magicDNSAddr, "100.64.0.1")[0]},
		}},
		MagicDNSDomain: "tail-scale.ts.net",
		RequireAuthKey: key,
	}
	control.HTTPTestServer = httptest.NewUnstartedServer(control)
	control.HTTPTestServer.Start()
	t.Cleanup(control.HTTPTestServer.Close)
	return control
}

type logSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, format+"\n", args...)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestEnrolRejectsBadKeyThenEnrolsAndRestartsWithoutKey(t *testing.T) {
	control := startTestControl(t, goodKey)
	stateDir := filepath.Join(t.TempDir(), "tsnet-state")
	logs := &logSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opts := Options{Hostname: "library", StateDir: stateDir, ControlURL: control.HTTPTestServer.URL, Logf: logs.logf}

	bad := opts
	bad.AuthKey = "tskey-auth-wrong"
	start := time.Now()
	if n, err := Start(ctx, bad); err == nil {
		_ = n.Close()
		t.Fatal("a wrong auth key enrolled")
	} else if !strings.Contains(err.Error(), "invalid authkey") {
		t.Fatalf("wrong key: err = %v", err)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("a wrong key took %s to fail", d)
	}

	good := opts
	good.AuthKey = goodKey
	n, err := Start(ctx, good)
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if n.FQDN() != "library.tail-scale.ts.net" || !strings.HasPrefix(n.TailscaleIP(), "100.") {
		t.Fatalf("fqdn=%q ip=%q", n.FQDN(), n.TailscaleIP())
	}
	if err := n.CheckMagicDNS(ctx); err != nil {
		t.Fatalf("MagicDNS self-check: %v", err)
	}
	_ = n.Close()
	if !Enrolled(stateDir) {
		t.Fatal("Enrolled = false after enrolment")
	}

	// A real coordination server accepts a known node key without an auth
	// key; this test server checks the key on every registration.
	control.RequireAuthKey = ""
	again, err := Start(ctx, opts)
	if err != nil {
		t.Fatalf("restart from saved state: %v", err)
	}
	if again.FQDN() != "library.tail-scale.ts.net" {
		t.Fatalf("restart fqdn = %q", again.FQDN())
	}
	_ = again.Close()

	if strings.Contains(logs.String(), goodKey) {
		t.Fatal("the auth key appeared in tsnet logs")
	}
	if err := filepath.Walk(filepath.Dir(stateDir), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), goodKey) {
			t.Errorf("auth key written to %s", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckMagicDNSRejectsARecordForAnotherNode(t *testing.T) {
	control := startTestControl(t, goodKey, "100.64.0.99")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	n, err := Start(ctx, Options{Hostname: "library", StateDir: t.TempDir(), AuthKey: goodKey, ControlURL: control.HTTPTestServer.URL, Logf: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	if err := n.CheckMagicDNS(ctx); err == nil || !strings.Contains(err.Error(), "not this node") {
		t.Fatalf("CheckMagicDNS = %v, want a not-this-node error", err)
	}
}

func TestCheckHTTPSHandshakesThroughTheTailnet(t *testing.T) {
	control := startTestControl(t, goodKey)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	n, err := Start(ctx, Options{Hostname: "library", StateDir: t.TempDir(), AuthKey: goodKey, ControlURL: control.HTTPTestServer.URL, Logf: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()

	// The test coordination server cannot issue certificates; stand in for
	// the LocalAPI fetch with a test CA's certificate for the node's name.
	_, roots, chainPEM := testCert(t, n.FQDN())
	n.certPair = func(_ context.Context, domain string) ([]byte, []byte, error) {
		if domain != n.FQDN() {
			return nil, nil, fmt.Errorf("asked for %q", domain)
		}
		return chainPEM, nil, nil
	}

	notAfter, err := n.CheckHTTPS(ctx, roots)
	if err != nil {
		t.Fatalf("CheckHTTPS: %v", err)
	}
	if time.Until(notAfter) < 30*time.Minute {
		t.Fatalf("notAfter = %v", notAfter)
	}
	if _, err := n.CheckHTTPS(ctx, x509.NewCertPool()); err == nil {
		t.Fatal("an untrusted certificate passed the HTTPS check")
	}
	_, _, otherPEM := testCert(t, "other.tail-scale.ts.net")
	n.certPair = func(context.Context, string) ([]byte, []byte, error) { return otherPEM, nil, nil }
	if _, err := n.CheckHTTPS(ctx, roots); err == nil {
		t.Fatal("a certificate for another name passed the HTTPS check")
	}
}

func testCert(t *testing.T, host string) (tls.Certificate, *x509.CertPool, []byte) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"}, NotBefore: time.Now().Add(-time.Minute),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	chainPEM := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}, pool, chainPEM
}

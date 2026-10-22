package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reagent/common"
	"reagent/config"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func init() {
	// No test may dial a real device endpoint to fold its chain into the
	// tunnel CA bundle; many carry production URLs. Set once, before any test
	// goroutine exists. Tests that need an endpoint chain call refreshTunnelCA
	// with their own source.
	tunnelEndpointCAs = func(string) ([][]byte, error) { return nil, nil }
}

// frpc verifies the frps certificate only when frpc.yaml names a
// trustedCaFile; without one it accepts any certificate. Cloud devices and
// domain-mode appliances must get one, pinned to the name they dial; plain-mode
// appliances (self-signed frps) and local dev stacks must not.
func TestTunnelTLSVerificationPerDeploymentMode(t *testing.T) {
	tests := []struct {
		name           string
		reswarm        *config.ReswarmConfig
		wantServerName string // "" means unverified
	}{
		{
			name: "cloud device verifies app.ironflock.com",
			reswarm: &config.ReswarmConfig{
				DeviceEndpointURL: "wss://cbw.ironflock.com/ws-re-dev",
				Environment:       string(common.PRODUCTION),
				Secret:            "device-cra-secret",
				DeviceKey:         2,
			},
			wantServerName: "app.ironflock.com",
		},
		{
			name:           "cloud device on the environment default verifies app.ironflock.com",
			reswarm:        &config.ReswarmConfig{Environment: string(common.PRODUCTION)},
			wantServerName: "app.ironflock.com",
		},
		{
			name: "domain-mode appliance verifies the appliance domain",
			reswarm: &config.ReswarmConfig{
				ApplianceDomain:   "tls-sf015.corp.example.com",
				DeviceEndpointURL: "wss://ws.tls-sf015.corp.example.com/ws-re-dev",
				Environment:       string(common.PRODUCTION),
			},
			wantServerName: "tls-sf015.corp.example.com",
		},
		{
			name: "plain-mode appliance dialed by IP stays unverified",
			reswarm: &config.ReswarmConfig{
				ApplianceDomain:   "192.168.0.21.nip.io",
				DeviceEndpointURL: "ws://192.168.0.21:18080/ws-re-dev",
				Environment:       string(common.PRODUCTION),
			},
		},
		{
			name: "plain-mode appliance dialed by name stays unverified",
			reswarm: &config.ReswarmConfig{
				ApplianceDomain:   "tls-sf015.corp.example.com",
				DeviceEndpointURL: "ws://tls-sf015/ws-re-dev",
				Environment:       string(common.PRODUCTION),
			},
		},
		{
			name:    "localhost dev stack stays unverified",
			reswarm: &config.ReswarmConfig{DeviceEndpointURL: "ws://localhost:8080/ws-re-dev", Environment: string(common.LOCAL)},
		},
		{
			name:    "containerized dev agent stays unverified",
			reswarm: &config.ReswarmConfig{DeviceEndpointURL: "ws://host.docker.internal:8080/ws-re-dev", Environment: string(common.LOCAL)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(TUNNEL_TLS_INSECURE_SKIP_VERIFY, "")
			cfg := builderConfig(t, tt.reswarm)
			builder := NewTunnelConfigBuilder(cfg)
			caPath := filepath.Join(cfg.CommandLineArguments.AgentDir, "tunnel-ca.pem")

			raw, err := os.ReadFile(builder.ConfigPath)
			require.NoError(t, err)
			var written FrpcYamlConfig
			require.NoError(t, yaml.Unmarshal(raw, &written))
			require.NotNil(t, written.Transport)
			require.NotNil(t, written.Transport.TLS)
			assert.True(t, written.Transport.TLS.Enable)

			if tt.wantServerName == "" {
				assert.Empty(t, written.Transport.TLS.TrustedCaFile)
				assert.Empty(t, written.Transport.TLS.ServerName)
				assert.NotContains(t, string(raw), "trustedCaFile")
				assert.NoFileExists(t, caPath)
				return
			}

			assert.Equal(t, caPath, written.Transport.TLS.TrustedCaFile)
			assert.Equal(t, tt.wantServerName, written.Transport.TLS.ServerName)
			assert.Contains(t, bundleSubjects(t, caPath), "ISRG Root X1", "the bundle frpc is pointed at must exist")

			if tt.reswarm.Secret != "" {
				assert.Equal(t, tunnelAuthProof(tt.reswarm.Secret, tt.reswarm.DeviceKey), written.Metadatas["tunnel_proof"],
					"verification must not touch the proof the frps plugin checks")
			}
		})
	}
}

// escapeHatchLogChild marks the child test binary that
// TestTunnelTLSInsecureSkipVerifyEscapeHatch runs itself in.
const escapeHatchLogChild = "TUNNEL_TLS_ESCAPE_HATCH_LOG_CHILD"

// The escape hatch restores the unverified connection and says so, loudly,
// every time the config is built rather than once.
//
// The warnings are read from a child test binary whose logger writes JSON to
// stderr: swapping the global logger in this process would race any goroutine
// another test left logging.
func TestTunnelTLSInsecureSkipVerifyEscapeHatch(t *testing.T) {
	if os.Getenv(escapeHatchLogChild) != "1" {
		child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1")
		child.Env = append(os.Environ(), escapeHatchLogChild+"=1")
		var logged, output bytes.Buffer
		child.Stderr = &logged
		child.Stdout = &output
		require.NoError(t, child.Run(), "%s\n%s", output.String(), logged.String())

		assert.Equal(t, 2, strings.Count(logged.String(), `"level":"warn","server":"app.ironflock.com","message":"WARNING: TUNNEL_TLS_INSECURE_SKIP_VERIFY is set`),
			"one warning per config build:\n%s", logged.String())
		return
	}

	log.Logger = zerolog.New(os.Stderr)
	cloud := &config.ReswarmConfig{DeviceEndpointURL: "wss://cbw.ironflock.com/ws-re-dev", Environment: string(common.PRODUCTION)}

	t.Setenv(TUNNEL_TLS_INSECURE_SKIP_VERIFY, "true")
	builder := NewTunnelConfigBuilder(builderConfig(t, cloud))
	assert.Empty(t, builder.yamlConfig.Transport.TLS.TrustedCaFile)
	assert.Empty(t, builder.yamlConfig.Transport.TLS.ServerName)

	builder.Reset()
	assert.Empty(t, builder.yamlConfig.Transport.TLS.TrustedCaFile)

	// Anything that is not a boolean must not be read as consent.
	t.Setenv(TUNNEL_TLS_INSECURE_SKIP_VERIFY, "please")
	builder = NewTunnelConfigBuilder(builderConfig(t, cloud))
	assert.NotEmpty(t, builder.yamlConfig.Transport.TLS.TrustedCaFile)
}

// TestFrpcVerifiesTunnelServerCertificate runs the real embedded frpc against a
// local TLS listener with the config initialize() produces for a cloud device,
// only redirected to the listener. It proves frpc honours the keys we write
// (it parses strictly) and that they verify: the right CA and name pass, an
// impersonating server is refused, and the escape hatch accepts it again.
func TestFrpcVerifiesTunnelServerCertificate(t *testing.T) {
	frpcBinary, err := filepath.Abs(filepath.Join("..", "embedded", "frpc_binary"))
	require.NoError(t, err)
	if out, err := exec.Command(frpcBinary, "-v").Output(); err != nil {
		t.Skipf("no runnable embedded frpc at %s: %v", frpcBinary, err)
	} else {
		t.Logf("frpc %s", strings.TrimSpace(string(out)))
	}

	trustedCA, trustedKey := testTunnelCA(t, "Corp Tunnel Root")
	rogueCA, rogueKey := testTunnelCA(t, "Rogue Root")

	tests := []struct {
		name         string
		insecure     bool
		serverCA     *x509.Certificate
		serverKey    *ecdsa.PrivateKey
		serverName   string
		trustTestCA  bool
		wantAccepted bool
	}{
		{"trusted CA and matching name is accepted", false, trustedCA, trustedKey, "app.ironflock.com", true, true},
		{"server outside the bundle is refused", false, rogueCA, rogueKey, "app.ironflock.com", true, false},
		{"trusted CA for another name is refused", false, trustedCA, trustedKey, "evil.example.com", true, false},
		{"escape hatch accepts the impersonating server", true, rogueCA, rogueKey, "app.ironflock.com", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearProxyEnv(t)
			t.Setenv(TUNNEL_TLS_INSECURE_SKIP_VERIFY, "")
			if tt.insecure {
				t.Setenv(TUNNEL_TLS_INSECURE_SKIP_VERIFY, "true")
			}

			cfg := builderConfig(t, &config.ReswarmConfig{DeviceEndpointURL: "wss://cbw.ironflock.com/ws-re-dev", Environment: string(common.PRODUCTION)})
			builder := NewTunnelConfigBuilder(cfg)

			listener, handshakes := tlsHandshakeRecorder(t, testLeaf(t, tt.serverCA, tt.serverKey, tt.serverName))

			// Same config, dialed at the listener. serverName stays
			// app.ironflock.com, so what is verified is unchanged.
			frpcConfig := *builder.yamlConfig
			frpcConfig.ServerAddr = "127.0.0.1"
			frpcConfig.ServerPort = listener.Addr().(*net.TCPAddr).Port
			frpcConfig.WebServer = nil
			frpcConfig.Log = &LogConfig{To: "console", Level: "debug", MaxDays: 1}
			data, err := yaml.Marshal(&frpcConfig)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(builder.ConfigPath, data, 0o644))

			if tt.trustTestCA {
				// As if the device's host store held the issuing root.
				caPath := frpcConfig.Transport.TLS.TrustedCaFile
				bundle, err := os.ReadFile(caPath)
				require.NoError(t, err)
				bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: trustedCA.Raw})...)
				require.NoError(t, os.WriteFile(caPath, bundle, 0o644))
			}

			ctx, cancel := context.WithCancel(context.Background())
			// frpc's output is copied in by exec's goroutines while the
			// assertions below may already read it.
			output := &syncBuffer{}
			cmd := exec.CommandContext(ctx, frpcBinary, "-c", builder.ConfigPath)
			cmd.Env = frpcEnv()
			cmd.Stdout = output
			cmd.Stderr = output
			require.NoError(t, cmd.Start())
			t.Cleanup(func() {
				cancel()
				_ = cmd.Wait()
			})

			select {
			case err := <-handshakes:
				if tt.wantAccepted {
					assert.NoError(t, err, "frpc must complete the handshake\n%s", output.String())
				} else {
					// A client alert, i.e. frpc rejected the certificate
					// rather than the connection failing some other way.
					assert.ErrorContains(t, err, "remote error: tls:", "frpc must refuse the certificate\n%s", output.String())
				}
				t.Logf("server handshake result: %v", err)
			case <-time.After(20 * time.Second):
				t.Fatalf("frpc never reached the listener\n%s", output.String())
			}
		})
	}
}

// syncBuffer is a bytes.Buffer safe for one writer and concurrent readers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func bundleSubjects(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var subjects []string
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return subjects
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)
		subjects = append(subjects, cert.Subject.CommonName)
	}
}

func testTunnelCA(t *testing.T, commonName string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert, key
}

func testLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dnsName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: key}
}

// tlsHandshakeRecorder serves cert on a loopback port and reports the outcome
// of every TLS handshake: nil when the client accepted the certificate, the
// client's alert when it refused it.
func tlsHandshakeRecorder(t *testing.T, cert tls.Certificate) (net.Listener, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })

	results := make(chan error, 16)
	serverConfig := &tls.Config{Certificates: []tls.Certificate{cert}}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

				// frp may announce TLS with a custom 0x17 first byte.
				reader := bufio.NewReader(conn)
				if first, err := reader.Peek(1); err == nil && first[0] == 0x17 {
					_, _ = reader.Discard(1)
				}

				err := tls.Server(&bufferedConn{Conn: conn, reader: reader}, serverConfig).Handshake()
				select {
				case results <- err:
				default:
				}
			}(conn)
		}
	}()

	return listener, results
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// Where frpc verifies, the capability probe completes a TLS handshake with the
// same trust, so a refused certificate surfaces as Unavailable with the x509
// reason and the escape hatch, not as "Available".
func TestProbeCapabilityReportsUntrustedTunnelServer(t *testing.T) {
	trustedCA, trustedKey := testTunnelCA(t, "Corp Tunnel Root")
	rogueCA, rogueKey := testTunnelCA(t, "Rogue Root")

	probe := func(t *testing.T, serverCA *x509.Certificate, serverKey *ecdsa.PrivateKey) (TunnelCapability, string) {
		m := newCapabilityTestManager(t)
		tlsConfig := m.configBuilder.yamlConfig.Transport.TLS
		require.NotEmpty(t, tlsConfig.TrustedCaFile, "a cloud config verifies")
		require.Equal(t, "app.ironflock.com", tlsConfig.ServerName)

		bundle, err := os.ReadFile(tlsConfig.TrustedCaFile)
		require.NoError(t, err)
		bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: trustedCA.Raw})...)
		require.NoError(t, os.WriteFile(tlsConfig.TrustedCaFile, bundle, 0o644))

		listener, _ := tlsHandshakeRecorder(t, testLeaf(t, serverCA, serverKey, "app.ironflock.com"))
		m.configBuilder.yamlConfig.ServerAddr = "127.0.0.1"
		m.configBuilder.yamlConfig.ServerPort = listener.Addr().(*net.TCPAddr).Port
		m.clientAlive.Store(true)
		fakeAdminAPI(t, m)

		m.probeCapability()
		return m.Capability()
	}

	t.Run("trusted server is available", func(t *testing.T) {
		capability, lastErr := probe(t, trustedCA, trustedKey)
		assert.Equal(t, CapabilityAvailable, capability, lastErr)
	})

	t.Run("impersonating server is unavailable with the reason", func(t *testing.T) {
		capability, lastErr := probe(t, rogueCA, rogueKey)
		assert.Equal(t, CapabilityUnavailable, capability)
		assert.Contains(t, lastErr, "x509: certificate signed by unknown authority")
		assert.Contains(t, lastErr, TUNNEL_TLS_INSECURE_SKIP_VERIFY)
		assert.NotContains(t, lastErr, "frps unreachable", "the server is reachable; its certificate is the problem")
	})
}

// A host store that cannot be read must not shrink the bundle to the embedded
// roots: the bundle on disk was built from the store and holds the corporate
// root. Only without an earlier bundle is one built without the store.
func TestRefreshTunnelCAKeepsBundleWhenHostStoreUnreadable(t *testing.T) {
	unreadable := func() ([][]byte, error) { return nil, errors.New("store locked") }
	noEndpoint := func() ([][]byte, error) { return nil, nil }

	t.Run("existing bundle is kept", func(t *testing.T) {
		caPath := filepath.Join(t.TempDir(), "tunnel-ca.pem")
		corpCA, _ := testTunnelCA(t, "Corp Root")
		earlier := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: corpCA.Raw})
		require.NoError(t, os.WriteFile(caPath, earlier, 0o644))

		refreshTunnelCA(caPath, unreadable, noEndpoint)

		kept, err := os.ReadFile(caPath)
		require.NoError(t, err)
		assert.Equal(t, earlier, kept)
	})

	t.Run("without one the embedded roots are written", func(t *testing.T) {
		caPath := filepath.Join(t.TempDir(), "tunnel-ca.pem")

		refreshTunnelCA(caPath, unreadable, noEndpoint)

		assert.ElementsMatch(t, []string{"ISRG Root X1", "ISRG Root X2", "Root YR", "Root YE"}, bundleSubjects(t, caPath))
	})
}

// The appliance ingress serves the WAMP endpoint and the frps apex with the
// same chain, so whatever chain the host verified for the endpoint must reach
// frpc's bundle, even when the exported store lacks it (Windows fetches
// intermediates and some roots on demand).
func TestRefreshTunnelCAIncludesEndpointChain(t *testing.T) {
	issuing, _ := testTunnelCA(t, "Corp Issuing CA")
	caPath := filepath.Join(t.TempDir(), "tunnel-ca.pem")

	refreshTunnelCA(caPath,
		func() ([][]byte, error) { return nil, nil },
		func() ([][]byte, error) { return [][]byte{issuing.Raw}, nil })

	assert.Contains(t, bundleSubjects(t, caPath), "Corp Issuing CA")
}

// frpc loads its CA bundle once, at startup, so Start must hand it a current
// one, not only SaveConfig.
func TestStartRebuildsTunnelCABundle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake frpc is a shell script")
	}

	m := newCapabilityTestManager(t)
	m.loginDeadline = 200 * time.Millisecond
	caPath := m.configBuilder.yamlConfig.Transport.TLS.TrustedCaFile
	require.NotEmpty(t, caPath)
	require.NoError(t, os.Remove(caPath))

	frpcPath := filepath.Join(m.config.CommandLineArguments.AgentDir, "frpc")
	require.NoError(t, os.WriteFile(frpcPath, []byte("#!/bin/sh\nsleep 2\n"), 0o755))
	t.Cleanup(func() { _ = m.Stop() })

	// The probe after start dials a closed port; the verdict is not the point.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	m.configBuilder.yamlConfig.ServerAddr = "127.0.0.1"
	m.configBuilder.yamlConfig.ServerPort = listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	require.NoError(t, m.Start())

	assert.Contains(t, bundleSubjects(t, caPath), "ISRG Root X1")
}

// The frpc config carries the device's tunnel_proof, so it must not be
// readable by other local users — also when an older agent left it 0644.
func TestFrpcConfigIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not meaningful on windows")
	}
	cloud := &config.ReswarmConfig{DeviceEndpointURL: "wss://cbw.ironflock.com/ws-re-dev", Environment: string(common.PRODUCTION), Secret: "s", DeviceKey: 7}
	cfg := builderConfig(t, cloud)
	builder := NewTunnelConfigBuilder(cfg)

	info, err := os.Stat(builder.ConfigPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "new frpc config")

	require.NoError(t, os.Chmod(builder.ConfigPath, 0o644))
	builder.SaveConfig()
	info, err = os.Stat(builder.ConfigPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "config left 0644 by an older agent")
}

/*
Copyright 2016-2019 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/net/http2"
	"golang.org/x/sync/errgroup"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/client/webclient"
	"github.com/gravitational/teleport/api/constants"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	devicepb "github.com/gravitational/teleport/api/gen/proto/go/teleport/devicetrust/v1"
	kubeproto "github.com/gravitational/teleport/api/gen/proto/go/teleport/kube/v1"
	apitracing "github.com/gravitational/teleport/api/observability/tracing"
	tracessh "github.com/gravitational/teleport/api/observability/tracing/ssh"
	"github.com/gravitational/teleport/api/profile"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	apiutils "github.com/gravitational/teleport/api/utils"
	"github.com/gravitational/teleport/api/utils/keys"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/native"
	"github.com/gravitational/teleport/lib/auth/touchid"
	wancli "github.com/gravitational/teleport/lib/auth/webauthncli"
	"github.com/gravitational/teleport/lib/client/terminal"
	"github.com/gravitational/teleport/lib/defaults"
	dtauthn "github.com/gravitational/teleport/lib/devicetrust/authn"
	"github.com/gravitational/teleport/lib/events"
	kubeutils "github.com/gravitational/teleport/lib/kube/utils"
	"github.com/gravitational/teleport/lib/modules"
	"github.com/gravitational/teleport/lib/observability/tracing"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/shell"
	alpncommon "github.com/gravitational/teleport/lib/srv/alpnproxy/common"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/sshutils/scp"
	"github.com/gravitational/teleport/lib/sshutils/sftp"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/teleport/lib/utils/agentconn"
	"github.com/gravitational/teleport/lib/utils/prompt"
	"github.com/gravitational/teleport/lib/utils/proxy"
)

const (
	AddKeysToAgentAuto = "auto"
	AddKeysToAgentNo   = "no"
	AddKeysToAgentYes  = "yes"
	AddKeysToAgentOnly = "only"
)

var AllAddKeysOptions = []string{AddKeysToAgentAuto, AddKeysToAgentNo, AddKeysToAgentYes, AddKeysToAgentOnly}

// ValidateAgentKeyOption validates that a string is a valid option for the AddKeysToAgent parameter.
func ValidateAgentKeyOption(supplied string) error {
	for _, option := range AllAddKeysOptions {
		if supplied == option {
			return nil
		}
	}

	return trace.BadParameter("invalid value %q, must be one of %v", supplied, AllAddKeysOptions)
}

// AgentForwardingMode  describes how the user key agent will be forwarded
// to a remote machine, if at all.
type AgentForwardingMode int

const (
	ForwardAgentNo AgentForwardingMode = iota
	ForwardAgentYes
	ForwardAgentLocal
)

var log = logrus.WithFields(logrus.Fields{
	trace.Component: teleport.ComponentClient,
})

// ForwardedPort specifies local tunnel to remote
// destination managed by the client, is equivalent
// of ssh -L src:host:dst command
type ForwardedPort struct {
	SrcIP    string
	SrcPort  int
	DestPort int
	DestHost string
}

// ForwardedPorts contains an array of forwarded port structs
type ForwardedPorts []ForwardedPort

// ToString returns a string representation of a forwarded port spec, compatible
// with OpenSSH's -L  flag, i.e. "src_host:src_port:dest_host:dest_port".
func (p *ForwardedPort) ToString() string {
	sport := strconv.Itoa(p.SrcPort)
	dport := strconv.Itoa(p.DestPort)
	if utils.IsLocalhost(p.SrcIP) {
		return sport + ":" + net.JoinHostPort(p.DestHost, dport)
	}
	return net.JoinHostPort(p.SrcIP, sport) + ":" + net.JoinHostPort(p.DestHost, dport)
}

// DynamicForwardedPort local port for dynamic application-level port
// forwarding. Whenever a connection is made to this port, SOCKS5 protocol
// is used to determine the address of the remote host. More or less
// equivalent to OpenSSH's -D flag.
type DynamicForwardedPort struct {
	// SrcIP is the IP address to listen on locally.
	SrcIP string

	// SrcPort is the port to listen on locally.
	SrcPort int
}

// DynamicForwardedPorts is a slice of locally forwarded dynamic ports (SOCKS5).
type DynamicForwardedPorts []DynamicForwardedPort

// ToString returns a string representation of a dynamic port spec, compatible
// with OpenSSH's -D flag, i.e. "src_host:src_port".
func (p *DynamicForwardedPort) ToString() string {
	sport := strconv.Itoa(p.SrcPort)
	if utils.IsLocalhost(p.SrcIP) {
		return sport
	}
	return net.JoinHostPort(p.SrcIP, sport)
}

// HostKeyCallback is called by SSH client when it needs to check
// remote host key or certificate validity
type HostKeyCallback func(host string, ip net.Addr, key ssh.PublicKey) error

// Config is a client config
type Config struct {
	// Username is the Teleport account username (for logging into Teleport proxies)
	Username string
	// ExplicitUsername is true if Username was initially set by the end-user
	// (for example, using command-line flags).
	ExplicitUsername bool

	// Remote host to connect
	Host string

	// SearchKeywords host to connect
	SearchKeywords []string

	// PredicateExpression host to connect
	PredicateExpression string

	// Labels represent host Labels
	Labels map[string]string

	// Namespace is nodes namespace
	Namespace string

	// HostLogin is a user login on a remote host
	HostLogin string

	// HostPort is a remote host port to connect to. This is used for **explicit**
	// port setting via -p flag, otherwise '0' is passed which means "use server default"
	HostPort int

	// JumpHosts if specified are interpreted in a similar way
	// as -J flag in ssh - used to dial through
	JumpHosts []utils.JumpHost

	// WebProxyAddr is the host:port the web proxy can be accessed at.
	WebProxyAddr string

	// SSHProxyAddr is the host:port the SSH proxy can be accessed at.
	SSHProxyAddr string

	// KubeProxyAddr is the host:port the Kubernetes proxy can be accessed at.
	KubeProxyAddr string

	// PostgresProxyAddr is the host:port the Postgres proxy can be accessed at.
	PostgresProxyAddr string

	// MongoProxyAddr is the host:port the Mongo proxy can be accessed at.
	MongoProxyAddr string

	// MySQLProxyAddr is the host:port the MySQL proxy can be accessed at.
	MySQLProxyAddr string

	// KeyTTL is a time to live for the temporary SSH keypair to remain valid:
	KeyTTL time.Duration

	// InsecureSkipVerify is an option to skip HTTPS cert check
	InsecureSkipVerify bool

	// SkipLocalAuth tells the client not to use its own SSH agent or ask user for passwords. This is
	// used by external programs linking against Teleport client and obtaining credentials from elsewhere.
	// e.g. from an identity file.
	SkipLocalAuth bool

	// UseKeyPrincipals forces the use of the username from the key principals rather than using
	// the current user username.
	UseKeyPrincipals bool

	// Agent is used when SkipLocalAuth is true
	Agent agent.ExtendedAgent

	ClientStore *Store

	// ForwardAgent is used by the client to request agent forwarding from the server.
	ForwardAgent AgentForwardingMode

	// EnableX11Forwarding specifies whether X11 forwarding should be enabled.
	EnableX11Forwarding bool

	// X11ForwardingTimeout can be set to set a X11 forwarding timeout in seconds,
	// after which any X11 forwarding requests in that session will be rejected.
	X11ForwardingTimeout time.Duration

	// X11ForwardingTrusted specifies the X11 forwarding security mode.
	X11ForwardingTrusted bool

	// AuthMethods are used to login into the cluster. If specified, the client will
	// use them in addition to certs stored in the client store.
	AuthMethods []ssh.AuthMethod

	// TLSConfig is TLS configuration, if specified, the client
	// will use this TLS configuration to access API endpoints
	TLS *tls.Config

	// DefaultPrincipal determines the default SSH username (principal) the client should be using
	// when connecting to auth/proxy servers. Usually it's returned with a certificate,
	// but this variables provides a default (used by the web-based terminal client)
	DefaultPrincipal string

	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader

	// ExitStatus carries the returned value (exit status) of the remote
	// process execution (via SSH exec)
	ExitStatus int

	// SiteName specifies site to execute operation,
	// if omitted, first available site will be selected
	SiteName string

	// KubernetesCluster specifies the kubernetes cluster for any relevant
	// operations. If empty, the auth server will choose one using stable (same
	// cluster every time) but unspecified logic.
	KubernetesCluster string

	// DatabaseService specifies name of the database proxy server to issue
	// certificate for.
	DatabaseService string

	// LocalForwardPorts are the local ports tsh listens on for port forwarding
	// (parameters to -L ssh flag).
	LocalForwardPorts ForwardedPorts

	// DynamicForwardedPorts are the list of ports tsh listens on for dynamic
	// port forwarding (parameters to -D ssh flag).
	DynamicForwardedPorts DynamicForwardedPorts

	// HostKeyCallback will be called to check host keys of the remote
	// node, if not specified will be using CheckHostSignature function
	// that uses local cache to validate hosts
	HostKeyCallback ssh.HostKeyCallback

	// KeyDir defines where temporary session keys will be stored.
	// if empty, they'll go to ~/.tsh
	KeysDir string

	// SessionID is a session ID to use when opening a new session.
	SessionID string

	// Interactive, when set to true, tells tsh to launch a remote command
	// in interactive mode, i.e. attaching the temrinal to it
	Interactive bool

	// ClientAddr (if set) specifies the true client IP. Usually it's not needed (since the server
	// can look at the connecting address to determine client's IP) but for cases when the
	// client is web-based, this must be set to HTTP's remote addr
	ClientAddr string

	// CachePolicy defines local caching policy in case if discovery goes down
	// by default does not use caching
	CachePolicy *CachePolicy

	// CertificateFormat is the format of the SSH certificate.
	CertificateFormat string

	// AuthConnector is the name of the authentication connector to use.
	AuthConnector string

	// AuthenticatorAttachment is the desired authenticator attachment.
	AuthenticatorAttachment wancli.AuthenticatorAttachment

	// PreferOTP prefers OTP in favor of other MFA methods.
	// Useful in constrained environments without access to USB or platform
	// authenticators, such as remote hosts or virtual machines.
	PreferOTP bool

	// CheckVersions will check that client version is compatible
	// with auth server version when connecting.
	CheckVersions bool

	// BindAddr is an optional host:port to bind to for SSO redirect flows.
	BindAddr string

	// NoRemoteExec will not execute a remote command after connecting to a host,
	// will block instead. Useful when port forwarding. Equivalent of -N for OpenSSH.
	NoRemoteExec bool

	// Browser can be used to pass the name of a browser to override the system default
	// (not currently implemented), or set to 'none' to suppress browser opening entirely.
	Browser string

	// AddKeysToAgent specifies how the client handles keys.
	//	auto - will attempt to add keys to agent if the agent supports it
	//	only - attempt to load keys into agent but don't write them to disk
	//	on - attempt to load keys into agent
	//	off - do not attempt to load keys into agent
	AddKeysToAgent string

	// EnableEscapeSequences will scan Stdin for SSH escape sequences during
	// command/shell execution. This also requires Stdin to be an interactive
	// terminal.
	EnableEscapeSequences bool

	// MockSSOLogin is used in tests for mocking the SSO login response.
	MockSSOLogin SSOLoginFunc

	// HomePath is where tsh stores profiles
	HomePath string

	// TLSRoutingEnabled indicates that proxy supports ALPN SNI server where
	// all proxy services are exposed on a single TLS listener (Proxy Web Listener).
	TLSRoutingEnabled bool

	// Reason is a reason attached to started sessions meant to describe their intent.
	Reason string

	// Invited is a list of people invited to a session.
	Invited []string

	// DisplayParticipantRequirements is set if debug information about participants requirements
	// should be printed in moderated sessions.
	DisplayParticipantRequirements bool

	// ExtraProxyHeaders is a collection of http headers to be included in requests to the WebProxy.
	ExtraProxyHeaders map[string]string

	// AllowStdinHijack allows stdin hijack during MFA prompts.
	// Stdin hijack provides a better login UX, but it can be difficult to reason
	// about and is often a source of bugs.
	// Do not set this options unless you deeply understand what you are doing.
	AllowStdinHijack bool

	// Tracer is the tracer to create spans with
	Tracer oteltrace.Tracer

	// PrivateKeyPolicy is a key policy that this client will try to follow during login.
	PrivateKeyPolicy keys.PrivateKeyPolicy

	// LoadAllCAs indicates that tsh should load the CAs of all clusters
	// instead of just the current cluster.
	LoadAllCAs bool
}

// CachePolicy defines cache policy for local clients
type CachePolicy struct {
	// CacheTTL defines cache TTL
	CacheTTL time.Duration
	// NeverExpire never expires local cache information
	NeverExpires bool
}

// MakeDefaultConfig returns default client config
func MakeDefaultConfig() *Config {
	return &Config{
		Stdout:                os.Stdout,
		Stderr:                os.Stderr,
		Stdin:                 os.Stdin,
		AddKeysToAgent:        AddKeysToAgentAuto,
		EnableEscapeSequences: true,
		Tracer:                tracing.NoopProvider().Tracer("TeleportClient"),
	}
}

// VirtualPathKind is the suffix component for env vars denoting the type of
// file that will be loaded.
type VirtualPathKind string

const (
	// VirtualPathEnvPrefix is the env var name prefix shared by all virtual
	// path vars.
	VirtualPathEnvPrefix = "TSH_VIRTUAL_PATH"

	VirtualPathKey        VirtualPathKind = "KEY"
	VirtualPathCA         VirtualPathKind = "CA"
	VirtualPathDatabase   VirtualPathKind = "DB"
	VirtualPathApp        VirtualPathKind = "APP"
	VirtualPathKubernetes VirtualPathKind = "KUBE"
)

// VirtualPathParams are an ordered list of additional optional parameters
// for a virtual path. They can be used to specify a more exact resource name
// if multiple might be available. Simpler integrations can instead only
// specify the kind and it will apply wherever a more specific env var isn't
// found.
type VirtualPathParams []string

// VirtualPathCAParams returns parameters for selecting CA certificates.
func VirtualPathCAParams(caType types.CertAuthType) VirtualPathParams {
	return VirtualPathParams{
		strings.ToUpper(string(caType)),
	}
}

// VirtualPathDatabaseParams returns parameters for selecting specific database
// certificates.
func VirtualPathDatabaseParams(databaseName string) VirtualPathParams {
	return VirtualPathParams{databaseName}
}

// VirtualPathAppParams returns parameters for selecting specific apps by name.
func VirtualPathAppParams(appName string) VirtualPathParams {
	return VirtualPathParams{appName}
}

// VirtualPathKubernetesParams returns parameters for selecting k8s clusters by
// name.
func VirtualPathKubernetesParams(k8sCluster string) VirtualPathParams {
	return VirtualPathParams{k8sCluster}
}

// VirtualPathEnvName formats a single virtual path environment variable name.
func VirtualPathEnvName(kind VirtualPathKind, params VirtualPathParams) string {
	components := append([]string{
		VirtualPathEnvPrefix,
		string(kind),
	}, params...)

	return strings.ToUpper(strings.Join(components, "_"))
}

// VirtualPathEnvNames determines an ordered list of environment variables that
// should be checked to resolve an env var override. Params may be nil to
// indicate no additional arguments are to be specified or accepted.
func VirtualPathEnvNames(kind VirtualPathKind, params VirtualPathParams) []string {
	// Bail out early if there are no parameters.
	if len(params) == 0 {
		return []string{VirtualPathEnvName(kind, VirtualPathParams{})}
	}

	var vars []string
	for i := len(params); i >= 0; i-- {
		vars = append(vars, VirtualPathEnvName(kind, params[0:i]))
	}

	return vars
}

// RetryWithRelogin is a helper error handling method, attempts to relogin and
// retry the function once.
func RetryWithRelogin(ctx context.Context, tc *TeleportClient, fn func() error) error {
	err := fn()
	if err == nil {
		return nil
	}

	if utils.IsPredicateError(err) {
		return trace.Wrap(utils.PredicateError{Err: err})
	}

	if !IsErrorResolvableWithRelogin(err) {
		return trace.Wrap(err)
	}

	// Don't try to login when using an identity file / external identity.
	if tc.SkipLocalAuth {
		return trace.Wrap(err)
	}

	log.Debugf("Activating relogin on %v.", err)

	// check if the error is a private key policy error.
	if privateKeyPolicy, err := keys.ParsePrivateKeyPolicyError(err); err == nil {
		// The current private key was rejected due to an unmet key policy requirement.
		fmt.Fprintf(tc.Stderr, "Unmet private key policy %q\n", privateKeyPolicy)
		fmt.Fprintf(tc.Stderr, "Relogging in with YubiKey generated private key.\n")

		// The current private key was rejected due to an unmet key policy requirement.
		// Set the private key policy to the expected value and re-login.
		tc.PrivateKeyPolicy = privateKeyPolicy
	}

	key, err := tc.Login(ctx)
	if err != nil {
		if trace.IsTrustError(err) {
			return trace.Wrap(err, "refusing to connect to untrusted proxy %v without --insecure flag\n", tc.SSHProxyAddr)
		}
		return trace.Wrap(err)
	}
	if err := tc.ActivateKey(ctx, key); err != nil {
		return trace.Wrap(err)
	}

	// Attempt device login. This activates a fresh key if successful.
	if err := tc.AttemptDeviceLogin(ctx, key); err != nil {
		return trace.Wrap(err)
	}

	// Save profile to record proxy credentials
	if err := tc.SaveProfile(true); err != nil {
		log.Warningf("Failed to save profile: %v", err)
		return trace.Wrap(err)
	}

	return fn()
}

func IsErrorResolvableWithRelogin(err error) bool {
	// Assume that failed handshake is a result of expired credentials.
	return utils.IsHandshakeFailedError(err) || utils.IsCertExpiredError(err) ||
		trace.IsBadParameter(err) || trace.IsTrustError(err) || keys.IsPrivateKeyPolicyError(err) || trace.IsNotFound(err)
}

// LoadProfile populates Config with the values stored in the given
// profiles directory. If profileDir is an empty string, the default profile
// directory ~/.tsh is used.
func (c *Config) LoadProfile(ps ProfileStore, proxyAddr string) error {
	var proxyHost string
	var err error
	if proxyAddr == "" {
		proxyHost, err = ps.CurrentProfile()
		if err != nil {
			return trace.Wrap(err)
		}
	} else {
		proxyHost, err = utils.Host(proxyAddr)
		if err != nil {
			return trace.Wrap(err)
		}
	}

	profile, err := ps.GetProfile(proxyHost)
	if err != nil {
		return trace.Wrap(err)
	}

	c.Username = profile.Username
	c.SiteName = profile.SiteName
	c.KubeProxyAddr = profile.KubeProxyAddr
	c.WebProxyAddr = profile.WebProxyAddr
	c.SSHProxyAddr = profile.SSHProxyAddr
	c.PostgresProxyAddr = profile.PostgresProxyAddr
	c.MySQLProxyAddr = profile.MySQLProxyAddr
	c.MongoProxyAddr = profile.MongoProxyAddr
	c.TLSRoutingEnabled = profile.TLSRoutingEnabled
	c.KeysDir = profile.Dir
	c.AuthConnector = profile.AuthConnector
	c.LoadAllCAs = profile.LoadAllCAs
	c.PrivateKeyPolicy = profile.PrivateKeyPolicy
	c.AuthenticatorAttachment, err = parseMFAMode(profile.MFAMode)
	if err != nil {
		return trace.BadParameter("unable to parse mfa mode in user profile: %v.", err)
	}

	c.LocalForwardPorts, err = ParsePortForwardSpec(profile.ForwardedPorts)
	if err != nil {
		log.Warnf("Unable to parse port forwarding in user profile: %v.", err)
	}

	c.DynamicForwardedPorts, err = ParseDynamicPortForwardSpec(profile.DynamicForwardedPorts)
	if err != nil {
		log.Warnf("Unable to parse dynamic port forwarding in user profile: %v.", err)
	}

	return nil
}

// SaveProfile updates the given profiles directory with the current configuration
// If profileDir is an empty string, the default ~/.tsh is used
func (c *Config) SaveProfile(makeCurrent bool) error {
	if c.WebProxyAddr == "" {
		return nil
	}

	p := &profile.Profile{
		Username:          c.Username,
		WebProxyAddr:      c.WebProxyAddr,
		SSHProxyAddr:      c.SSHProxyAddr,
		KubeProxyAddr:     c.KubeProxyAddr,
		PostgresProxyAddr: c.PostgresProxyAddr,
		MySQLProxyAddr:    c.MySQLProxyAddr,
		MongoProxyAddr:    c.MongoProxyAddr,
		ForwardedPorts:    c.LocalForwardPorts.String(),
		SiteName:          c.SiteName,
		TLSRoutingEnabled: c.TLSRoutingEnabled,
		AuthConnector:     c.AuthConnector,
		MFAMode:           c.AuthenticatorAttachment.String(),
		LoadAllCAs:        c.LoadAllCAs,
		PrivateKeyPolicy:  c.PrivateKeyPolicy,
	}

	if err := c.ClientStore.SaveProfile(p, makeCurrent); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// ParsedProxyHost holds the hostname and Web & SSH proxy addresses
// parsed out of a WebProxyAddress string.
type ParsedProxyHost struct {
	Host string

	// UsingDefaultWebProxyPort means that the port in WebProxyAddr was
	// supplied by ParseProxyHost function rather than ProxyHost string
	// itself.
	UsingDefaultWebProxyPort bool
	WebProxyAddr             string
	SSHProxyAddr             string
}

// ParseProxyHost parses a ProxyHost string of the format <hostname>:<proxy_web_port>,<proxy_ssh_port>
// and returns the parsed components.
//
// There are several "default" ports that the Web Proxy service may use, and if the port is not
// specified in the supplied proxyHost string
//
// If a definitive answer is not possible (e.g.  no proxy port is specified in
// the supplied string), ParseProxyHost() will supply default versions and flag
// that a default value is being used in the returned `ParsedProxyHost`
func ParseProxyHost(proxyHost string) (*ParsedProxyHost, error) {
	host, port, err := net.SplitHostPort(proxyHost)
	if err != nil {
		host = proxyHost
		port = ""
	}

	// set the default values of the port strings. One, both, or neither may
	// be overridden by the port string parsing below.
	usingDefaultWebProxyPort := true
	webPort := strconv.Itoa(defaults.HTTPListenPort)
	sshPort := strconv.Itoa(defaults.SSHProxyListenPort)

	// Split the port string out into at most two parts, the proxy port and
	// ssh port. Any more that 2 parts will be considered an error.
	parts := strings.Split(port, ",")

	switch {
	// Default ports for both the SSH and Web proxy.
	case len(parts) == 0:
		break

	// User defined HTTP proxy port, default SSH proxy port.
	case len(parts) == 1:
		if text := strings.TrimSpace(parts[0]); len(text) > 0 {
			webPort = text
			usingDefaultWebProxyPort = false
		}

	// User defined HTTP and SSH proxy ports.
	case len(parts) == 2:
		if text := strings.TrimSpace(parts[0]); len(text) > 0 {
			webPort = text
			usingDefaultWebProxyPort = false
		}
		if text := strings.TrimSpace(parts[1]); len(text) > 0 {
			sshPort = text
		}

	default:
		return nil, trace.BadParameter("unable to parse port: %v", port)
	}

	result := &ParsedProxyHost{
		Host:                     host,
		UsingDefaultWebProxyPort: usingDefaultWebProxyPort,
		WebProxyAddr:             net.JoinHostPort(host, webPort),
		SSHProxyAddr:             net.JoinHostPort(host, sshPort),
	}
	return result, nil
}

// ParseProxyHost parses the proxyHost string and updates the config.
//
// Format of proxyHost string:
//
//	proxy_web_addr:<proxy_web_port>,<proxy_ssh_port>
func (c *Config) ParseProxyHost(proxyHost string) error {
	parsedAddrs, err := ParseProxyHost(proxyHost)
	if err != nil {
		return trace.Wrap(err)
	}
	c.WebProxyAddr = parsedAddrs.WebProxyAddr
	c.SSHProxyAddr = parsedAddrs.SSHProxyAddr
	return nil
}

// KubeProxyHostPort returns the host and port of the Kubernetes proxy.
func (c *Config) KubeProxyHostPort() (string, int) {
	if c.KubeProxyAddr != "" {
		addr, err := utils.ParseAddr(c.KubeProxyAddr)
		if err == nil {
			return addr.Host(), addr.Port(defaults.KubeListenPort)
		}
	}

	webProxyHost, _ := c.WebProxyHostPort()
	return webProxyHost, defaults.KubeListenPort
}

// KubeClusterAddr returns a public HTTPS address of the proxy for use by
// Kubernetes client.
func (c *Config) KubeClusterAddr() string {
	host, port := c.KubeProxyHostPort()
	return fmt.Sprintf("https://%s:%d", host, port)
}

// WebProxyHostPort returns the host and port of the web proxy.
func (c *Config) WebProxyHostPort() (string, int) {
	if c.WebProxyAddr != "" {
		addr, err := utils.ParseAddr(c.WebProxyAddr)
		if err == nil {
			return addr.Host(), addr.Port(defaults.HTTPListenPort)
		}
	}
	return "unknown", defaults.HTTPListenPort
}

// WebProxyHost returns the web proxy host without the port number.
func (c *Config) WebProxyHost() string {
	host, _ := c.WebProxyHostPort()
	return host
}

// WebProxyPort returns the port of the web proxy.
func (c *Config) WebProxyPort() int {
	_, port := c.WebProxyHostPort()
	return port
}

// SSHProxyHostPort returns the host and port of the SSH proxy.
func (c *Config) SSHProxyHostPort() (string, int) {
	if c.SSHProxyAddr != "" {
		addr, err := utils.ParseAddr(c.SSHProxyAddr)
		if err == nil {
			return addr.Host(), addr.Port(defaults.SSHProxyListenPort)
		}
	}

	webProxyHost, _ := c.WebProxyHostPort()
	return webProxyHost, defaults.SSHProxyListenPort
}

// PostgresProxyHostPort returns the host and port of Postgres proxy.
func (c *Config) PostgresProxyHostPort() (string, int) {
	if c.PostgresProxyAddr != "" {
		addr, err := utils.ParseAddr(c.PostgresProxyAddr)
		if err == nil {
			return addr.Host(), addr.Port(c.WebProxyPort())
		}
	}
	return c.WebProxyHostPort()
}

// MongoProxyHostPort returns the host and port of Mongo proxy.
func (c *Config) MongoProxyHostPort() (string, int) {
	if c.MongoProxyAddr != "" {
		addr, err := utils.ParseAddr(c.MongoProxyAddr)
		if err == nil {
			return addr.Host(), addr.Port(defaults.MongoListenPort)
		}
	}
	return c.WebProxyHostPort()
}

// MySQLProxyHostPort returns the host and port of MySQL proxy.
func (c *Config) MySQLProxyHostPort() (string, int) {
	if c.MySQLProxyAddr != "" {
		addr, err := utils.ParseAddr(c.MySQLProxyAddr)
		if err == nil {
			return addr.Host(), addr.Port(defaults.MySQLListenPort)
		}
	}
	webProxyHost, _ := c.WebProxyHostPort()
	return webProxyHost, defaults.MySQLListenPort
}

// DatabaseProxyHostPort returns proxy connection endpoint for the database.
func (c *Config) DatabaseProxyHostPort(db tlsca.RouteToDatabase) (string, int) {
	switch db.Protocol {
	case defaults.ProtocolPostgres, defaults.ProtocolCockroachDB:
		return c.PostgresProxyHostPort()
	case defaults.ProtocolMySQL:
		return c.MySQLProxyHostPort()
	case defaults.ProtocolMongoDB:
		return c.MongoProxyHostPort()
	}
	return c.WebProxyHostPort()
}

// GetKubeTLSServerName returns k8s server name used in KUBECONFIG to leverage TLS Routing.
func GetKubeTLSServerName(k8host string) string {
	isIPFormat := net.ParseIP(k8host) != nil

	if k8host == "" || isIPFormat {
		// If proxy is configured without public_addr set the ServerName to the 'kube.teleport.cluster.local' value.
		// The k8s server name needs to be a valid hostname but when public_addr is missing from proxy settings
		// the web_listen_addr is used thus webHost will contain local proxy IP address like: 0.0.0.0 or 127.0.0.1
		return addSubdomainPrefix(constants.APIDomain, constants.KubeTeleportProxyALPNPrefix)
	}
	return addSubdomainPrefix(k8host, constants.KubeTeleportProxyALPNPrefix)
}

// GetOldKubeTLSServerName returns k8s server name used in KUBECONFIG to leverage TLS Routing.
// TODO(smallinsky) DELETE IN 14.0.0 After dropping support for KubeSNIPrefix SNI routing handler.
func GetOldKubeTLSServerName(k8host string) string {
	isIPFormat := net.ParseIP(k8host) != nil

	if k8host == "" || isIPFormat {
		// If proxy is configured without public_addr set the ServerName to the 'kube.teleport.cluster.local' value.
		// The k8s server name needs to be a valid hostname but when public_addr is missing from proxy settings
		// the web_listen_addr is used thus webHost will contain local proxy IP address like: 0.0.0.0 or 127.0.0.1
		return addSubdomainPrefix(constants.APIDomain, constants.KubeSNIPrefix)
	}
	return addSubdomainPrefix(k8host, constants.KubeSNIPrefix)
}

func addSubdomainPrefix(domain, prefix string) string {
	return fmt.Sprintf("%s%s", prefix, domain)
}

// ProxyHost returns the hostname of the proxy server (without any port numbers)
func ProxyHost(proxyHost string) string {
	host, _, err := net.SplitHostPort(proxyHost)
	if err != nil {
		return proxyHost
	}
	return host
}

// ProxySpecified returns true if proxy has been specified.
func (c *Config) ProxySpecified() bool {
	return c.WebProxyAddr != ""
}

// DefaultResourceFilter returns the default list resource request.
func (c *Config) DefaultResourceFilter() *proto.ListResourcesRequest {
	return &proto.ListResourcesRequest{
		Namespace:           c.Namespace,
		Labels:              c.Labels,
		SearchKeywords:      c.SearchKeywords,
		PredicateExpression: c.PredicateExpression,
	}
}

// dtAuthnRunCeremonyFunc matches the signature of [dtauthn.RunCeremony].
type dtAuthnRunCeremonyFunc func(context.Context, devicepb.DeviceTrustServiceClient, *devicepb.UserCertificates) (*devicepb.UserCertificates, error)

// TeleportClient is a wrapper around SSH client with teleport specific
// workflow built in.
// TeleportClient is NOT safe for concurrent use.
type TeleportClient struct {
	Config
	localAgent *LocalKeyAgent

	// OnShellCreated gets called when the shell is created. It's
	// safe to keep it nil.
	OnShellCreated ShellCreatedCallback

	// eventsCh is a channel used to inform clients about events have that
	// occurred during the session.
	eventsCh chan events.EventFields

	// Note: there's no mutex guarding this or localAgent, making
	// TeleportClient NOT safe for concurrent use.
	lastPing *webclient.PingResponse

	// dtAttemptLoginIgnorePing allows tests to override AttemptDeviceLogin's Ping
	// response validation.
	dtAttemptLoginIgnorePing bool

	// dtAuthnRunCeremony allows tests to override the default device
	// authentication function.
	// Defaults to [dtauthn.RunCeremony].
	dtAuthnRunCeremony dtAuthnRunCeremonyFunc
}

// ShellCreatedCallback can be supplied for every teleport client. It will
// be called right after the remote shell is created, but the session
// hasn't begun yet.
//
// It allows clients to cancel SSH action
type ShellCreatedCallback func(s *tracessh.Session, c *tracessh.Client, terminal io.ReadWriteCloser) (exit bool, err error)

// NewClient creates a TeleportClient object and fully configures it
func NewClient(c *Config) (tc *TeleportClient, err error) {
	if len(c.JumpHosts) > 1 {
		return nil, trace.BadParameter("only one jump host is supported, got %v", len(c.JumpHosts))
	}
	// validate configuration
	if c.Username == "" {
		c.Username, err = Username()
		if err != nil {
			return nil, trace.Wrap(err)
		}
		log.Infof("No teleport login given. defaulting to %s", c.Username)
	}
	if c.WebProxyAddr == "" {
		return nil, trace.BadParameter("No proxy address specified, missed --proxy flag?")
	}
	if c.HostLogin == "" {
		c.HostLogin, err = Username()
		if err != nil {
			return nil, trace.Wrap(err)
		}
		log.Infof("no host login given. defaulting to %s", c.HostLogin)
	}
	if c.KeyTTL == 0 {
		c.KeyTTL = apidefaults.CertDuration
	}
	c.Namespace = types.ProcessNamespace(c.Namespace)

	if c.Tracer == nil {
		c.Tracer = tracing.NoopProvider().Tracer(teleport.ComponentTeleport)
	}

	tc = &TeleportClient{
		Config: *c,
	}

	if tc.Stdout == nil {
		tc.Stdout = os.Stdout
	}
	if tc.Stderr == nil {
		tc.Stderr = os.Stderr
	}
	if tc.Stdin == nil {
		tc.Stdin = os.Stdin
	}

	if tc.ClientStore == nil {
		if c.SkipLocalAuth {
			// initialize empty client store to prevent panics.
			tc.ClientStore = NewMemClientStore()
		} else {
			tc.ClientStore = NewFSClientStore(c.KeysDir)
			if c.AddKeysToAgent == AddKeysToAgentOnly {
				// Store client keys in memory, but still save trusted certs and profile to disk.
				tc.ClientStore.KeyStore = NewMemKeyStore()
			}
		}
	}

	// Create a buffered channel to hold events that occurred during this session.
	// This channel must be buffered because the SSH connection directly feeds
	// into it. Delays in pulling messages off the global SSH request channel
	// could lead to the connection hanging.
	tc.eventsCh = make(chan events.EventFields, 1024)

	localAgentCfg := LocalAgentConfig{
		ClientStore: tc.ClientStore,
		Agent:       c.Agent,
		ProxyHost:   tc.WebProxyHost(),
		Username:    c.Username,
		KeysOption:  c.AddKeysToAgent,
		Insecure:    c.InsecureSkipVerify,
		Site:        tc.SiteName,
		LoadAllCAs:  tc.LoadAllCAs,
	}

	// initialize the local agent (auth agent which uses local SSH keys signed by the CA):
	tc.localAgent, err = NewLocalAgent(localAgentCfg)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if tc.HostKeyCallback == nil {
		tc.HostKeyCallback = tc.localAgent.HostKeyCallback
	}

	return tc, nil
}

func (tc *TeleportClient) ProfileStatus() (*ProfileStatus, error) {
	status, err := tc.ClientStore.ReadProfileStatus(tc.WebProxyAddr)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// If the profile has a different username than the current client, don't return
	// the profile. This is used for login and logout logic.
	if status.Username != tc.Username {
		return nil, trace.NotFound("no profile for proxy %v and user %v found", tc.WebProxyAddr, tc.Username)
	}
	return status, nil
}

// LoadKeyForCluster fetches a cluster-specific SSH key and loads it into the
// SSH agent.
func (tc *TeleportClient) LoadKeyForCluster(clusterName string) error {
	if tc.localAgent == nil {
		return trace.BadParameter("TeleportClient.LoadKeyForCluster called on a client without localAgent")
	}
	if err := tc.localAgent.LoadKeyForCluster(clusterName); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// LoadKeyForClusterWithReissue fetches a cluster-specific SSH key and loads it into the
// SSH agent.  If the key is not found, it is requested to be reissued.
func (tc *TeleportClient) LoadKeyForClusterWithReissue(ctx context.Context, clusterName string) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/LoadKeyForClusterWithReissue",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(attribute.String("cluster", clusterName)),
	)
	defer span.End()

	err := tc.LoadKeyForCluster(clusterName)
	if err == nil {
		return nil
	}
	if !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	// Reissuing also loads the new key.
	err = tc.ReissueUserCerts(ctx, CertCacheKeep, ReissueParams{RouteToCluster: clusterName})
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// SignersForClusterWithReissue fetches cluster-specific signers from stored certificates.
// If the cluster certificates are not found, it is requested to be reissued.
func (tc *TeleportClient) SignersForClusterWithReissue(ctx context.Context, clusterName string) ([]ssh.Signer, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/LoadKeyForClusterWithReissue",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(attribute.String("cluster", clusterName)),
	)
	defer span.End()

	signers, err := tc.localAgent.signersForCluster(clusterName)
	if err == nil {
		return signers, nil
	}
	if !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}

	if err := tc.WithoutJumpHosts(func(tc *TeleportClient) error {
		return tc.ReissueUserCerts(ctx, CertCacheKeep, ReissueParams{RouteToCluster: clusterName})
	}); err != nil {
		return nil, trace.Wrap(err)
	}

	signers, err = tc.localAgent.signersForCluster(clusterName)
	if err != nil {
		log.WithError(err).Warnf("Failed to load/reissue certificates for cluster %q.", clusterName)
		return nil, trace.Wrap(err)
	}
	return signers, nil
}

// LocalAgent is a getter function for the client's local agent
func (tc *TeleportClient) LocalAgent() *LocalKeyAgent {
	return tc.localAgent
}

// RootClusterName returns root cluster name.
func (tc *TeleportClient) RootClusterName(ctx context.Context) (string, error) {
	_, span := tc.Tracer.Start(
		ctx,
		"teleportClient/RootClusterName",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	key, err := tc.LocalAgent().GetCoreKey()
	if err != nil {
		return "", trace.Wrap(err)
	}
	name, err := key.RootClusterName()
	if err != nil {
		return "", trace.Wrap(err)
	}
	return name, nil
}

// getTargetNodes returns a list of node addresses this SSH command needs to
// operate on.
func (tc *TeleportClient) getTargetNodes(ctx context.Context, proxy *ProxyClient) ([]string, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/getTargetNodes",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	// use the target node that was explicitly provided if valid
	if len(tc.Labels) == 0 {
		// detect the common error when users use host:port address format
		_, port, err := net.SplitHostPort(tc.Host)
		// client has used host:port notation
		if err == nil {
			return nil, trace.BadParameter("please use ssh subcommand with '--port=%v' flag instead of semicolon", port)
		}

		addr := net.JoinHostPort(tc.Host, strconv.Itoa(tc.HostPort))
		return []string{addr}, nil
	}

	// find the nodes matching the labels that were provided
	nodes, err := proxy.FindNodesByFilters(ctx, *tc.DefaultResourceFilter())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	retval := make([]string, 0, len(nodes))
	for i := 0; i < len(nodes); i++ {
		// always dial nodes by UUID
		retval = append(retval, fmt.Sprintf("%s:0", nodes[i].GetName()))
	}

	return retval, nil
}

// ReissueUserCerts issues new user certs based on params and stores them in
// the local key agent (usually on disk in ~/.tsh).
func (tc *TeleportClient) ReissueUserCerts(ctx context.Context, cachePolicy CertCachePolicy, params ReissueParams) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/ReissueUserCerts",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	defer proxyClient.Close()

	err = RetryWithRelogin(ctx, tc, func() error {
		err := proxyClient.ReissueUserCerts(ctx, cachePolicy, params)
		return trace.Wrap(err)
	})
	return trace.Wrap(err)
}

// IssueUserCertsWithMFA issues a single-use SSH or TLS certificate for
// connecting to a target (node/k8s/db/app) specified in params with an MFA
// check. A user has to be logged in, there should be a valid login cert
// available.
//
// If access to this target does not require per-connection MFA checks
// (according to RBAC), IssueCertsWithMFA will:
// - for SSH certs, return the existing Key from the keystore.
// - for TLS certs, fall back to ReissueUserCerts.
func (tc *TeleportClient) IssueUserCertsWithMFA(ctx context.Context, params ReissueParams, applyOpts func(opts *PromptMFAChallengeOpts)) (*Key, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/IssueUserCertsWithMFA",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()

	return proxyClient.IssueUserCertsWithMFA(
		ctx, params,
		func(ctx context.Context, proxyAddr string, c *proto.MFAAuthenticateChallenge) (*proto.MFAAuthenticateResponse, error) {
			return tc.PromptMFAChallenge(ctx, proxyAddr, c, applyOpts)
		})
}

// CreateAccessRequest registers a new access request with the auth server.
func (tc *TeleportClient) CreateAccessRequest(ctx context.Context, req types.AccessRequest) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/CreateAccessRequest",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(attribute.String("request", req.GetName())),
	)
	defer span.End()

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	defer proxyClient.Close()

	return proxyClient.CreateAccessRequest(ctx, req)
}

// GetAccessRequests loads all access requests matching the supplied filter.
func (tc *TeleportClient) GetAccessRequests(ctx context.Context, filter types.AccessRequestFilter) ([]types.AccessRequest, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/GetAccessRequests",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			attribute.String("id", filter.ID),
			attribute.String("user", filter.User),
		),
	)
	defer span.End()

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()

	return proxyClient.GetAccessRequests(ctx, filter)
}

// GetRole loads a role resource by name.
func (tc *TeleportClient) GetRole(ctx context.Context, name string) (types.Role, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/GetRole",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			attribute.String("role", name),
		),
	)
	defer span.End()

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()

	return proxyClient.GetRole(ctx, name)
}

// watchCloser is a wrapper around a services.Watcher
// which holds a closer that must be called after the watcher
// is closed.
type watchCloser struct {
	types.Watcher
	io.Closer
}

func (w watchCloser) Close() error {
	return trace.NewAggregate(w.Watcher.Close(), w.Closer.Close())
}

// NewWatcher sets up a new event watcher.
func (tc *TeleportClient) NewWatcher(ctx context.Context, watch types.Watch) (types.Watcher, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/NewWatcher",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			attribute.String("name", watch.Name),
		),
	)
	defer span.End()

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	watcher, err := proxyClient.NewWatcher(ctx, watch)
	if err != nil {
		proxyClient.Close()
		return nil, trace.Wrap(err)
	}

	return watchCloser{
		Watcher: watcher,
		Closer:  proxyClient,
	}, nil
}

// WithRootClusterClient provides a functional interface for making calls
// against the root cluster's auth server.
func (tc *TeleportClient) WithRootClusterClient(ctx context.Context, do func(clt auth.ClientI) error) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/WithRootClusterClient",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	defer proxyClient.Close()

	clt, err := proxyClient.ConnectToRootCluster(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	defer clt.Close()

	return trace.Wrap(do(clt))
}

// NewTracingClient provides a tracing client that will forward spans on to
// the current clusters auth server. The auth server will then forward along to the configured
// telemetry backend.
func (tc *TeleportClient) NewTracingClient(ctx context.Context) (*apitracing.Client, error) {
	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	clt, err := proxyClient.NewTracingClient(ctx, tc.SiteName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return clt, nil
}

// SSH connects to a node and, if 'command' is specified, executes the command on it,
// otherwise runs interactive shell
//
// Returns nil if successful, or (possibly) *exec.ExitError
func (tc *TeleportClient) SSH(ctx context.Context, command []string, runLocally bool) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/SSH",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			attribute.String("proxy", tc.Config.WebProxyAddr),
		),
	)
	defer span.End()

	// connect to proxy first:
	if !tc.Config.ProxySpecified() {
		return trace.BadParameter("proxy server is not specified")
	}
	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	defer proxyClient.Close()

	// which nodes are we executing this commands on?
	nodeAddrs, err := tc.getTargetNodes(ctx, proxyClient)
	if err != nil {
		return trace.Wrap(err)
	}
	if len(nodeAddrs) == 0 {
		return trace.BadParameter("no target host specified")
	}

	if len(nodeAddrs) > 1 {
		return tc.runShellOrCommandOnMultipleNodes(ctx, nodeAddrs, proxyClient, command)
	}
	return tc.runShellOrCommandOnSingleNode(ctx, nodeAddrs[0], proxyClient, command, runLocally)
}

// ConnectToNode attempts to establish a connection to the node resolved to by the provided
// NodeDetails. If the connection fails due to an Access Denied error, Auth is queried to
// determine if per-session MFA is required for the node. If it is required then the MFA
// ceremony is performed and another connection is attempted with the freshly minted
// certificates. If it is not required, then the original Access Denied error from the node
// is returned.
func (tc *TeleportClient) ConnectToNode(ctx context.Context, proxyClient *ProxyClient, nodeDetails NodeDetails, user string) (*NodeClient, error) {
	node := nodeName(nodeDetails.Addr)
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/ConnectToNode",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			attribute.String("site", nodeDetails.Cluster),
			attribute.String("node", node),
		),
	)
	defer span.End()

	// attempt to use the existing credentials first
	authMethods := proxyClient.authMethods

	// if per-session mfa is required, perform the mfa ceremony to get
	// new certificates and use them instead
	if nodeDetails.MFACheck != nil && nodeDetails.MFACheck.Required {
		am, err := proxyClient.sessionSSHCertificate(ctx, nodeDetails)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		authMethods = am
	}

	// grab the cluster details
	details, err := proxyClient.clusterDetails(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// try connecting to the node
	nodeClient, connectErr := proxyClient.ConnectToNode(ctx, nodeDetails, user, details, authMethods)
	switch {
	case connectErr == nil: // no error return client
		return nodeClient, nil
	case nodeDetails.MFACheck != nil: // per-session mfa ceremony was already performed, return the results
		return nodeClient, trace.Wrap(connectErr)
	case connectErr != nil && !trace.IsAccessDenied(connectErr): // catastrophic error, return it
		return nil, trace.Wrap(connectErr)
	}

	// access was denied, determine if it was because per-session mfa is required
	clt, err := proxyClient.ConnectToCluster(ctx, nodeDetails.Cluster)
	if err != nil {
		// return the connection error instead of any errors from connecting to auth
		return nil, trace.Wrap(connectErr)
	}

	check, err := clt.IsMFARequired(ctx, &proto.IsMFARequiredRequest{
		Target: &proto.IsMFARequiredRequest_Node{
			Node: &proto.NodeLogin{
				Node:  node,
				Login: proxyClient.hostLogin,
			},
		},
	})
	if err != nil {
		return nil, trace.Wrap(connectErr)
	}

	// per-session mfa isn't required, the user simply does not
	// have access to the provided node
	if !check.Required {
		return nil, trace.Wrap(connectErr)
	}

	// per-session mfa is required, perform the mfa ceremony
	key, err := proxyClient.IssueUserCertsWithMFA(
		ctx,
		ReissueParams{
			NodeName:       node,
			RouteToCluster: nodeDetails.Cluster,
			MFACheck:       check,
			AuthClient:     clt,
		},
		func(ctx context.Context, proxyAddr string, c *proto.MFAAuthenticateChallenge) (*proto.MFAAuthenticateResponse, error) {
			return tc.PromptMFAChallenge(ctx, proxyAddr, c, nil /* applyOpts */)
		},
	)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// try connecting to the node again with the newly acquired certificates
	newAuthMethods, err := key.AsAuthMethod()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	nodeClient, err = proxyClient.ConnectToNode(ctx, nodeDetails, user, details, []ssh.AuthMethod{newAuthMethods})
	return nodeClient, trace.Wrap(err)
}

func (tc *TeleportClient) runShellOrCommandOnSingleNode(ctx context.Context, nodeAddr string, proxyClient *ProxyClient, command []string, runLocally bool) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/runShellOrCommandOnSingleNode",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			attribute.String("site", tc.SiteName),
			attribute.String("node", nodeAddr),
		),
	)
	defer span.End()

	nodeClient, err := tc.ConnectToNode(
		ctx,
		proxyClient,
		NodeDetails{Addr: nodeAddr, Namespace: tc.Namespace, Cluster: tc.SiteName},
		tc.Config.HostLogin,
	)
	if err != nil {
		tc.ExitStatus = 1
		return trace.Wrap(err)
	}
	defer nodeClient.Close()
	// If forwarding ports were specified, start port forwarding.
	if err := tc.startPortForwarding(ctx, nodeClient); err != nil {
		return trace.Wrap(err)
	}

	// If no remote command execution was requested, block on the context which
	// will unblock upon error or SIGINT.
	if tc.NoRemoteExec {
		log.Debugf("Connected to node, no remote command execution was requested, blocking until context closes.")
		<-ctx.Done()

		// Only return an error if the context was canceled by something other than SIGINT.
		if ctx.Err() != context.Canceled {
			return ctx.Err()
		}
		return nil
	}

	// After port forwarding, run a local command that uses the connection, and
	// then disconnect.
	if runLocally {
		if len(tc.Config.LocalForwardPorts) == 0 {
			fmt.Println("Executing command locally without connecting to any servers. This makes no sense.")
		}
		return runLocalCommand(command)
	}

	if len(command) > 0 {
		// Reuse the existing nodeClient we connected above.
		return tc.runCommand(ctx, nodeClient, command)
	}
	return tc.runShell(ctx, nodeClient, types.SessionPeerMode, nil, nil)
}

func (tc *TeleportClient) runShellOrCommandOnMultipleNodes(ctx context.Context, nodeAddrs []string, proxyClient *ProxyClient, command []string) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/runShellOrCommandOnMultipleNodes",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			attribute.String("site", tc.SiteName),
			attribute.StringSlice("node", nodeAddrs),
		),
	)
	defer span.End()

	// There was a command provided, run a non-interactive session against each match
	if len(command) > 0 {
		fmt.Printf("\x1b[1mWARNING\x1b[0m: Multiple nodes matched label selector, running command on all.\n")
		return tc.runCommandOnNodes(ctx, tc.SiteName, nodeAddrs, proxyClient, command)
	}

	// Issue "shell" request to the first matching node.
	fmt.Printf("\x1b[1mWARNING\x1b[0m: Multiple nodes match the label selector, picking first: %q\n", nodeAddrs[0])
	nodeClient, err := tc.ConnectToNode(
		ctx,
		proxyClient,
		NodeDetails{Addr: nodeAddrs[0], Namespace: tc.Namespace, Cluster: tc.SiteName},
		tc.Config.HostLogin,
	)
	if err != nil {
		tc.ExitStatus = 1
		return trace.Wrap(err)
	}
	defer nodeClient.Close()
	return tc.runShell(ctx, nodeClient, types.SessionPeerMode, nil, nil)
}

func (tc *TeleportClient) startPortForwarding(ctx context.Context, nodeClient *NodeClient) error {
	for _, fp := range tc.Config.LocalForwardPorts {
		addr := net.JoinHostPort(fp.SrcIP, strconv.Itoa(fp.SrcPort))
		socket, err := net.Listen("tcp", addr)
		if err != nil {
			return trace.Errorf("Failed to bind to %v: %v.", addr, err)
		}
		go nodeClient.listenAndForward(ctx, socket, addr, net.JoinHostPort(fp.DestHost, strconv.Itoa(fp.DestPort)))
	}
	for _, fp := range tc.Config.DynamicForwardedPorts {
		addr := net.JoinHostPort(fp.SrcIP, strconv.Itoa(fp.SrcPort))
		socket, err := net.Listen("tcp", addr)
		if err != nil {
			return trace.Errorf("Failed to bind to %v: %v.", addr, err)
		}
		go nodeClient.dynamicListenAndForward(ctx, socket, addr)
	}
	return nil
}

// Join connects to the existing/active SSH session
func (tc *TeleportClient) Join(ctx context.Context, mode types.SessionParticipantMode, namespace string, sessionID session.ID, input io.Reader) (err error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/Join",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			attribute.String("session", sessionID.String()),
			attribute.String("mode", string(mode)),
		),
	)
	defer span.End()

	if namespace == "" {
		return trace.BadParameter(auth.MissingNamespaceError)
	}
	tc.Stdin = input
	if sessionID.Check() != nil {
		return trace.Errorf("Invalid session ID format: %s", string(sessionID))
	}

	// connect to proxy:
	if !tc.Config.ProxySpecified() {
		return trace.BadParameter("proxy server is not specified")
	}
	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	defer proxyClient.Close()
	site := proxyClient.CurrentCluster()

	// Session joining is not supported in proxy recording mode
	if recConfig, err := site.GetSessionRecordingConfig(ctx); err != nil {
		// If the user can't see the recording mode, just let them try joining below
		if !trace.IsAccessDenied(err) {
			return trace.Wrap(err)
		}
	} else if services.IsRecordAtProxy(recConfig.GetMode()) {
		return trace.BadParameter("session joining is not supported in proxy recording mode")
	}

	session, err := site.GetSessionTracker(ctx, string(sessionID))
	if err != nil {
		if trace.IsNotFound(err) {
			return trace.NotFound("session %q not found or it has ended", sessionID)
		}
		return trace.Wrap(err)
	}

	if session.GetSessionKind() != types.SSHSessionKind {
		return trace.BadParameter("session joining is only supported for ssh sessions, not %q sessions", session.GetSessionKind())
	}

	// connect to server:
	nc, err := tc.ConnectToNode(ctx,
		proxyClient,
		NodeDetails{Addr: session.GetAddress() + ":0", Namespace: tc.Namespace, Cluster: tc.SiteName},
		tc.Config.HostLogin,
	)
	if err != nil {
		return trace.Wrap(err)
	}
	defer nc.Close()

	// Start forwarding ports if configured.
	if err := tc.startPortForwarding(ctx, nc); err != nil {
		return trace.Wrap(err)
	}

	presenceCtx, presenceCancel := context.WithCancel(ctx)
	defer presenceCancel()

	var beforeStart func(io.Writer)
	if mode == types.SessionModeratorMode {
		beforeStart = func(out io.Writer) {
			nc.OnMFA = func() {
				runPresenceTask(presenceCtx, out, site, tc, session.GetSessionID())
			}
		}
	}

	// running shell with a given session means "join" it:
	err = tc.runShell(ctx, nc, mode, session, beforeStart)
	return trace.Wrap(err)
}

// Play replays the recorded session
func (tc *TeleportClient) Play(ctx context.Context, namespace, sessionID string) (err error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/Play",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			attribute.String("session", sessionID),
		),
	)
	defer span.End()

	var sessionEvents []events.EventFields
	var stream []byte
	if namespace == "" {
		return trace.BadParameter(auth.MissingNamespaceError)
	}
	sid, err := session.ParseID(sessionID)
	if err != nil {
		return fmt.Errorf("'%v' is not a valid session ID (must be GUID)", sid)
	}
	// connect to the auth server (site) who made the recording
	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	defer proxyClient.Close()

	site := proxyClient.CurrentCluster()

	// request events for that session (to get timing data)
	sessionEvents, err = site.GetSessionEvents(namespace, *sid, 0, true)
	if err != nil {
		return trace.Wrap(err)
	}

	// Return an error if it is a desktop session
	if len(sessionEvents) > 0 {
		if sessionEvents[0].GetType() == events.WindowsDesktopSessionStartEvent {
			url := getDesktopEventWebURL(tc.localAgent.proxyHost, proxyClient.siteName, sid, sessionEvents)
			message := "Desktop sessions cannot be viewed with tsh." +
				" Please use the browser to play this session." +
				" Click on the URL to view the session in the browser:"
			return trace.BadParameter("%s\n%s", message, url)
		}
	}

	// read the stream into a buffer:
	for {
		tmp, err := site.GetSessionChunk(namespace, *sid, len(stream), events.MaxChunkBytes)
		if err != nil {
			return trace.Wrap(err)
		}
		if len(tmp) == 0 {
			break
		}
		stream = append(stream, tmp...)
	}

	return playSession(sessionEvents, stream)
}

func (tc *TeleportClient) GetSessionEvents(ctx context.Context, namespace, sessionID string) ([]events.EventFields, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/GetSessionEvents",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			attribute.String("session", sessionID),
		),
	)
	defer span.End()

	if namespace == "" {
		return nil, trace.BadParameter(auth.MissingNamespaceError)
	}
	sid, err := session.ParseID(sessionID)
	if err != nil {
		return nil, trace.BadParameter("%q is not a valid session ID (must be GUID)", sid)
	}
	// connect to the auth server (site) who made the recording
	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()

	site := proxyClient.CurrentCluster()

	events, err := site.GetSessionEvents(namespace, *sid, 0, true)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return events, nil
}

// PlayFile plays the recorded session from a tar file
func PlayFile(ctx context.Context, tarFile io.Reader, sid string) error {
	var sessionEvents []events.EventFields
	var stream []byte
	protoReader := events.NewProtoReader(tarFile)
	playbackDir, err := os.MkdirTemp("", "playback")
	if err != nil {
		return trace.Wrap(err)
	}
	defer os.RemoveAll(playbackDir)
	w, err := events.WriteForSSHPlayback(ctx, session.ID(sid), protoReader, playbackDir)
	if err != nil {
		return trace.Wrap(err)
	}
	sessionEvents, err = w.SessionEvents()
	if err != nil {
		return trace.Wrap(err)
	}
	stream, err = w.SessionChunks()
	if err != nil {
		return trace.Wrap(err)
	}

	return playSession(sessionEvents, stream)
}

// ExecuteSCP executes SCP command. It executes scp.Command using
// lower-level API integrations that mimic SCP CLI command behavior
func (tc *TeleportClient) ExecuteSCP(ctx context.Context, cmd scp.Command) (err error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/ExecuteSCP",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	// connect to proxy first:
	if !tc.Config.ProxySpecified() {
		return trace.BadParameter("proxy server is not specified")
	}

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	defer proxyClient.Close()

	// which nodes are we executing this commands on?
	nodeAddrs, err := tc.getTargetNodes(ctx, proxyClient)
	if err != nil {
		return trace.Wrap(err)
	}
	if len(nodeAddrs) == 0 {
		return trace.BadParameter("no target host specified")
	}

	nodeClient, err := tc.ConnectToNode(
		ctx,
		proxyClient,
		NodeDetails{Addr: nodeAddrs[0], Namespace: tc.Namespace, Cluster: tc.SiteName},
		tc.Config.HostLogin,
	)
	if err != nil {
		tc.ExitStatus = 1
		return trace.Wrap(err)
	}

	err = nodeClient.ExecuteSCP(ctx, cmd)
	if err != nil {
		// converts SSH error code to tc.ExitStatus
		exitError, _ := trace.Unwrap(err).(*ssh.ExitError)
		if exitError != nil {
			tc.ExitStatus = exitError.ExitStatus()
		}
		return err

	}

	return nil
}

// SFTP securely copies files between Nodes or SSH servers using SFTP
func (tc *TeleportClient) SFTP(ctx context.Context, args []string, port int, opts sftp.Options, quiet bool) (err error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/SFTP",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	if len(args) < 2 {
		return trace.Errorf("local and remote destinations are required")
	}
	first := args[0]
	last := args[len(args)-1]

	// local copy?
	if !isRemoteDest(first) && !isRemoteDest(last) {
		return trace.BadParameter("no remote destination specified")
	}

	var config *sftpConfig
	if isRemoteDest(last) {
		config, err = tc.uploadConfig(args, port, opts)
		if err != nil {
			return trace.Wrap(err)
		}
	} else {
		config, err = tc.downloadConfig(args, port, opts)
		if err != nil {
			return trace.Wrap(err)
		}
	}
	if config.hostLogin == "" {
		config.hostLogin = tc.Config.HostLogin
	}

	if !quiet {
		config.cfg.ProgressStream = func(fileInfo os.FileInfo) io.ReadWriter {
			return sftp.NewProgressBar(fileInfo.Size(), fileInfo.Name(), tc.Stdout)
		}
	}

	return trace.Wrap(tc.TransferFiles(ctx, config.hostLogin, config.addr, config.cfg))
}

type sftpConfig struct {
	cfg       *sftp.Config
	addr      string
	hostLogin string
}

func (tc *TeleportClient) uploadConfig(args []string, port int, opts sftp.Options) (*sftpConfig, error) {
	// args are guaranteed to have len(args) > 1
	srcPaths := args[:len(args)-1]
	// copy everything except the last arg (the destination)
	dstPath := args[len(args)-1]

	dst, addr, err := getSCPDestination(dstPath, port)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	cfg, err := sftp.CreateUploadConfig(srcPaths, dst.Path, opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &sftpConfig{
		cfg:       cfg,
		addr:      addr,
		hostLogin: dst.Login,
	}, nil
}

func (tc *TeleportClient) downloadConfig(args []string, port int, opts sftp.Options) (*sftpConfig, error) {
	if len(args) > 2 {
		return nil, trace.BadParameter("only one source file is supported when downloading files")
	}

	// args are guaranteed to have len(args) > 1
	src, addr, err := getSCPDestination(args[0], port)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	cfg, err := sftp.CreateDownloadConfig(src.Path, args[1], opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &sftpConfig{
		cfg:       cfg,
		addr:      addr,
		hostLogin: src.Login,
	}, nil
}

func getSCPDestination(target string, port int) (dest *scp.Destination, addr string, err error) {
	dest, err = scp.ParseSCPDestination(target)
	if err != nil {
		return nil, "", trace.Wrap(err)
	}
	addr = net.JoinHostPort(dest.Host.Host(), strconv.Itoa(port))
	return dest, addr, nil
}

// TransferFiles copies files between the current machine and the
// specified Node using the supplied config
func (tc *TeleportClient) TransferFiles(ctx context.Context, hostLogin, nodeAddr string, cfg *sftp.Config) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/TransferFiles",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	if hostLogin == "" {
		return trace.BadParameter("host login is not specified")
	}
	if nodeAddr == "" {
		return trace.BadParameter("node address is not specified")
	}

	if !tc.Config.ProxySpecified() {
		return trace.BadParameter("proxy server is not specified")
	}
	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	defer proxyClient.Close()

	client, err := tc.ConnectToNode(
		ctx,
		proxyClient,
		NodeDetails{Addr: nodeAddr, Namespace: tc.Namespace, Cluster: tc.SiteName},
		hostLogin,
	)
	if err != nil {
		return trace.Wrap(err)
	}

	return trace.Wrap(client.TransferFiles(ctx, cfg))
}

func isRemoteDest(name string) bool {
	return strings.ContainsRune(name, ':')
}

// ListNodesWithFilters returns a list of nodes connected to a proxy
func (tc *TeleportClient) ListNodesWithFilters(ctx context.Context) ([]types.Server, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/ListNodesWithFilters",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	// connect to the proxy and ask it to return a full list of servers
	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()

	servers, err := proxyClient.FindNodesByFilters(ctx, *tc.DefaultResourceFilter())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return servers, nil
}

// GetClusterAlerts returns a list of matching alerts from the current cluster.
func (tc *TeleportClient) GetClusterAlerts(ctx context.Context, req types.GetClusterAlertsRequest) ([]types.ClusterAlert, error) {
	ctx, span := tc.Tracer.Start(ctx,
		"teleportClient/GetClusterAlerts",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()

	alerts, err := proxyClient.GetClusterAlerts(ctx, req)
	return alerts, trace.Wrap(err)
}

// ListNodesWithFiltersAllClusters returns a map of all nodes in all clusters connected to this proxy.
func (tc *TeleportClient) ListNodesWithFiltersAllClusters(ctx context.Context) (map[string][]types.Server, error) {
	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()

	clusters, err := proxyClient.GetSites(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	servers := make(map[string][]types.Server, len(clusters))
	for _, cluster := range clusters {
		s, err := proxyClient.FindNodesByFiltersForCluster(ctx, *tc.DefaultResourceFilter(), cluster.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		servers[cluster.Name] = s
	}
	return servers, nil
}

// ListAppServersWithFilters returns a list of application servers.
func (tc *TeleportClient) ListAppServersWithFilters(ctx context.Context, customFilter *proto.ListResourcesRequest) ([]types.AppServer, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/ListAppServersWithFilters",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()

	filter := customFilter
	if filter == nil {
		filter = tc.DefaultResourceFilter()
	}

	servers, err := proxyClient.FindAppServersByFilters(ctx, *filter)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return servers, nil
}

// listAppServersWithFiltersAllClusters returns a map of all app servers in all clusters connected to this proxy.
func (tc *TeleportClient) listAppServersWithFiltersAllClusters(ctx context.Context, customFilter *proto.ListResourcesRequest) (map[string][]types.AppServer, error) {
	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()

	filter := customFilter
	if customFilter == nil {
		filter = tc.DefaultResourceFilter()
	}

	clusters, err := proxyClient.GetSites(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	servers := make(map[string][]types.AppServer, len(clusters))
	for _, cluster := range clusters {
		s, err := proxyClient.FindAppServersByFiltersForCluster(ctx, *filter, cluster.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		servers[cluster.Name] = s
	}
	return servers, nil
}

// ListApps returns all registered applications.
func (tc *TeleportClient) ListApps(ctx context.Context, customFilter *proto.ListResourcesRequest) ([]types.Application, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/ListApps",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	servers, err := tc.ListAppServersWithFilters(ctx, customFilter)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var apps []types.Application
	for _, server := range servers {
		apps = append(apps, server.GetApp())
	}
	return types.DeduplicateApps(apps), nil
}

// ListAppsAllClusters returns all registered applications across all clusters.
func (tc *TeleportClient) ListAppsAllClusters(ctx context.Context, customFilter *proto.ListResourcesRequest) (map[string][]types.Application, error) {
	serversByCluster, err := tc.listAppServersWithFiltersAllClusters(ctx, customFilter)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	clusters := make(map[string][]types.Application, len(serversByCluster))
	for cluster, servers := range serversByCluster {
		var apps []types.Application
		for _, server := range servers {
			apps = append(apps, server.GetApp())
		}
		clusters[cluster] = types.DeduplicateApps(apps)
	}
	return clusters, nil
}

// CreateAppSession creates a new application access session.
func (tc *TeleportClient) CreateAppSession(ctx context.Context, req types.CreateAppSessionRequest) (types.WebSession, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/CreateAppSession",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()
	return proxyClient.CreateAppSession(ctx, req)
}

// GetAppSession returns an existing application access session.
func (tc *TeleportClient) GetAppSession(ctx context.Context, req types.GetAppSessionRequest) (types.WebSession, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/GetAppSession",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			attribute.String("session", req.SessionID),
		),
	)
	defer span.End()

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()
	return proxyClient.GetAppSession(ctx, req)
}

// DeleteAppSession removes the specified application access session.
func (tc *TeleportClient) DeleteAppSession(ctx context.Context, sessionID string) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/DeleteAppSession",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	defer proxyClient.Close()
	return proxyClient.DeleteAppSession(ctx, sessionID)
}

// ListDatabaseServersWithFilters returns all registered database proxy servers.
func (tc *TeleportClient) ListDatabaseServersWithFilters(ctx context.Context, customFilter *proto.ListResourcesRequest) ([]types.DatabaseServer, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/ListDatabaseServersWithFilters",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()

	filter := customFilter
	if filter == nil {
		filter = tc.DefaultResourceFilter()
	}

	servers, err := proxyClient.FindDatabaseServersByFilters(ctx, *filter)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return servers, nil
}

// listDatabaseServersWithFilters returns all registered database proxy servers across all clusters.
func (tc *TeleportClient) listDatabaseServersWithFiltersAllClusters(ctx context.Context, customFilter *proto.ListResourcesRequest) (map[string][]types.DatabaseServer, error) {
	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()

	filter := customFilter
	if customFilter == nil {
		filter = tc.DefaultResourceFilter()
	}

	clusters, err := proxyClient.GetSites(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	servers := make(map[string][]types.DatabaseServer, len(clusters))
	for _, cluster := range clusters {
		s, err := proxyClient.FindDatabaseServersByFiltersForCluster(ctx, *filter, cluster.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		servers[cluster.Name] = s
	}
	return servers, nil
}

// ListDatabases returns all registered databases.
func (tc *TeleportClient) ListDatabases(ctx context.Context, customFilter *proto.ListResourcesRequest) ([]types.Database, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/ListDatabases",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	servers, err := tc.ListDatabaseServersWithFilters(ctx, customFilter)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var databases []types.Database
	for _, server := range servers {
		databases = append(databases, server.GetDatabase())
	}
	return types.DeduplicateDatabases(databases), nil
}

// ListDatabasesAllClusters returns all registered databases across all clusters.
func (tc *TeleportClient) ListDatabasesAllClusters(ctx context.Context, customFilter *proto.ListResourcesRequest) (map[string][]types.Database, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/ListDatabasesAllClusters",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	serversByCluster, err := tc.listDatabaseServersWithFiltersAllClusters(ctx, customFilter)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	clusters := make(map[string][]types.Database, len(serversByCluster))
	for cluster, servers := range serversByCluster {
		var databases []types.Database
		for _, server := range servers {
			databases = append(databases, server.GetDatabase())
		}
		clusters[cluster] = types.DeduplicateDatabases(databases)
	}
	return clusters, nil
}

// ListAllNodes is the same as ListNodes except that it ignores labels.
func (tc *TeleportClient) ListAllNodes(ctx context.Context) ([]types.Server, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/ListAllNodes",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()

	return proxyClient.FindNodesByFilters(ctx, proto.ListResourcesRequest{
		Namespace: tc.Namespace,
	})
}

// ListKubernetesClustersWithFiltersAllClusters returns a map of all kube clusters in all clusters connected to a proxy.
func (tc *TeleportClient) ListKubernetesClustersWithFiltersAllClusters(ctx context.Context, req proto.ListResourcesRequest) (map[string][]types.KubeCluster, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/ListKubernetesClustersWithFiltersAllClusters",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	pc, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	clusters, err := pc.GetSites(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	kubeClusters := make(map[string][]types.KubeCluster, 0)
	for _, cluster := range clusters {
		ac, err := pc.ConnectToCluster(ctx, cluster.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		kc, err := kubeutils.ListKubeClustersWithFilters(ctx, ac, req)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		kubeClusters[cluster.Name] = kc
	}

	return kubeClusters, nil
}

// roleGetter retrieves roles for the current user
type roleGetter interface {
	GetRoles(ctx context.Context) ([]types.Role, error)
}

// commandLimit determines how many commands may be executed in parallel.
// The limit will one of the following:
//   - 1 if per session mfa is required
//   - 1 if we cannot determine the users role set
//   - half the max connection limit defined by the users role set
//
// Out of an abundance of caution we only use half the max connection
// limit to allow other connections to be established.
func commandLimit(ctx context.Context, getter roleGetter, mfaRequired bool) int {
	if mfaRequired {
		return 1
	}

	roles, err := getter.GetRoles(ctx)
	if err != nil {
		return 1
	}

	max := services.NewRoleSet(roles...).MaxConnections()
	limit := max / 2

	switch {
	case max == 0:
		return -1
	case max == 1:
		return 1
	case limit <= 0:
		return 1
	default:
		return int(limit)
	}
}

// runCommandOnNodes executes a given bash command on a bunch of remote nodes.
func (tc *TeleportClient) runCommandOnNodes(ctx context.Context, siteName string, nodeAddresses []string, proxyClient *ProxyClient, command []string) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/runCommandOnNodes",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	clt, err := proxyClient.ConnectToCluster(ctx, siteName)
	if err != nil {
		return trace.Wrap(err)
	}
	defer clt.Close()

	// Let's check if the first node requires mfa.
	// If it's required, run commands sequentially to avoid
	// race conditions and weird ux during mfa.
	mfaRequiredCheck, err := clt.IsMFARequired(ctx, &proto.IsMFARequiredRequest{
		Target: &proto.IsMFARequiredRequest_Node{
			Node: &proto.NodeLogin{
				Node:  nodeName(nodeAddresses[0]),
				Login: proxyClient.hostLogin,
			},
		},
	})
	if err != nil {
		return trace.Wrap(err)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(commandLimit(ctx, clt, mfaRequiredCheck.Required))
	for _, address := range nodeAddresses {
		address := address
		g.Go(func() error {
			ctx, span := tc.Tracer.Start(
				gctx,
				"teleportClient/executingCommand",
				oteltrace.WithSpanKind(oteltrace.SpanKindClient),
				oteltrace.WithAttributes(attribute.String("node", address)),
			)
			defer span.End()

			nodeClient, err := tc.ConnectToNode(
				ctx,
				proxyClient,
				NodeDetails{
					Addr:      address,
					Namespace: tc.Namespace,
					Cluster:   siteName,
					MFACheck:  mfaRequiredCheck,
				},
				tc.Config.HostLogin,
			)
			if err != nil {
				fmt.Fprintln(tc.Stderr, err)
				return trace.Wrap(err)
			}
			defer nodeClient.Close()

			fmt.Printf("Running command on %v:\n", nodeName(address))

			return trace.Wrap(tc.runCommand(ctx, nodeClient, command))
		})
	}

	return trace.Wrap(g.Wait())
}

// runCommand executes a given bash command on an established NodeClient.
func (tc *TeleportClient) runCommand(ctx context.Context, nodeClient *NodeClient, command []string) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/runCommand",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	nodeSession, err := newSession(ctx, nodeClient, nil, tc.newSessionEnv(), tc.Stdin, tc.Stdout, tc.Stderr, tc.EnableEscapeSequences)
	if err != nil {
		return trace.Wrap(err)
	}
	defer nodeSession.Close()
	if err := nodeSession.runCommand(ctx, types.SessionPeerMode, command, tc.OnShellCreated, tc.Config.Interactive); err != nil {
		originErr := trace.Unwrap(err)
		exitErr, ok := originErr.(*ssh.ExitError)
		if ok {
			tc.ExitStatus = exitErr.ExitStatus()
		} else {
			// if an error occurs, but no exit status is passed back, GoSSH returns
			// a generic error like this. in this case the error message is printed
			// to stderr by the remote process so we have to quietly return 1:
			if strings.Contains(originErr.Error(), "exited without exit status") {
				tc.ExitStatus = 1
			}
		}

		return trace.Wrap(err)
	}

	return nil
}

func (tc *TeleportClient) newSessionEnv() map[string]string {
	env := map[string]string{
		teleport.SSHSessionWebproxyAddr: tc.WebProxyAddr,
	}
	if tc.SessionID != "" {
		env[sshutils.SessionEnvVar] = tc.SessionID
	}
	return env
}

// runShell starts an interactive SSH session/shell.
// sessionID : when empty, creates a new shell. otherwise it tries to join the existing session.
func (tc *TeleportClient) runShell(ctx context.Context, nodeClient *NodeClient, mode types.SessionParticipantMode, sessToJoin types.SessionTracker, beforeStart func(io.Writer)) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/runShell",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	env := tc.newSessionEnv()
	env[teleport.EnvSSHJoinMode] = string(mode)
	env[teleport.EnvSSHSessionReason] = tc.Config.Reason
	env[teleport.EnvSSHSessionDisplayParticipantRequirements] = strconv.FormatBool(tc.Config.DisplayParticipantRequirements)

	encoded, err := json.Marshal(&tc.Config.Invited)
	if err != nil {
		return trace.Wrap(err)
	}
	env[teleport.EnvSSHSessionInvited] = string(encoded)

	nodeSession, err := newSession(ctx, nodeClient, sessToJoin, env, tc.Stdin, tc.Stdout, tc.Stderr, tc.EnableEscapeSequences)
	if err != nil {
		return trace.Wrap(err)
	}
	if err = nodeSession.runShell(ctx, mode, beforeStart, tc.OnShellCreated); err != nil {
		switch e := trace.Unwrap(err).(type) {
		case *ssh.ExitError:
			tc.ExitStatus = e.ExitStatus()
		case *ssh.ExitMissingError:
			tc.ExitStatus = 1
		}

		return trace.Wrap(err)
	}
	if nodeSession.ExitMsg == "" {
		fmt.Fprintln(tc.Stderr, "the connection was closed on the remote side on ", time.Now().Format(time.RFC822))
	} else {
		fmt.Fprintln(tc.Stderr, nodeSession.ExitMsg)
	}
	return nil
}

// getProxyLogin determines which SSH principal to use when connecting to proxy.
func (tc *TeleportClient) getProxySSHPrincipal() string {
	proxyPrincipal := tc.Config.HostLogin
	if tc.DefaultPrincipal != "" {
		proxyPrincipal = tc.DefaultPrincipal
	}
	if len(tc.JumpHosts) > 1 && tc.JumpHosts[0].Username != "" {
		log.Debugf("Setting proxy login to jump host's parameter user %q", tc.JumpHosts[0].Username)
		proxyPrincipal = tc.JumpHosts[0].Username
	}
	// see if we already have a signed key in the cache, we'll use that instead
	if (!tc.Config.SkipLocalAuth || tc.UseKeyPrincipals) && tc.localAgent != nil {
		signers, err := tc.localAgent.Signers()
		if err != nil || len(signers) == 0 {
			return proxyPrincipal
		}
		cert, ok := signers[0].PublicKey().(*ssh.Certificate)
		if ok && len(cert.ValidPrincipals) > 0 {
			return cert.ValidPrincipals[0]
		}
	}
	return proxyPrincipal
}

const unconfiguredPublicAddrMsg = `WARNING:

The following error has occurred as Teleport does not recognize the address
that is being used to connect to it. This usually indicates that the
'public_addr' configuration option of the 'proxy_service' has not been
set to match the address you are hosting the proxy on.

If 'public_addr' is configured correctly, this could be an indicator of an
attempted man-in-the-middle attack.
`

// formatConnectToProxyErr adds additional user actionable advice to errors
// that are raised during ConnectToProxy.
func formatConnectToProxyErr(err error) error {
	if err == nil {
		return nil
	}

	// Handles the error that occurs when you connect to the Proxy SSH service
	// and the Proxy does not have a correct `public_addr` configured, and the
	// system is configured with non-multiplexed ports.
	if utils.IsHandshakeFailedError(err) {
		const principalStr = "not in the set of valid principals for given certificate"
		if strings.Contains(err.Error(), principalStr) {
			return trace.Wrap(err, unconfiguredPublicAddrMsg)
		}
	}

	return err
}

// ConnectToProxy will dial to the proxy server and return a ProxyClient when
// successful. If the passed in context is canceled, this function will return
// a trace.ConnectionProblem right away.
func (tc *TeleportClient) ConnectToProxy(ctx context.Context) (*ProxyClient, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/ConnectToProxy",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			attribute.String("proxy", tc.Config.WebProxyAddr),
		),
	)
	defer span.End()

	var err error
	var proxyClient *ProxyClient

	// Use connectContext and the cancel function to signal when a response is
	// returned from connectToProxy.
	connectContext, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		proxyClient, err = tc.connectToProxy(ctx)
	}()

	select {
	// ConnectToProxy returned a result, return that back to the caller.
	case <-connectContext.Done():
		return proxyClient, trace.Wrap(formatConnectToProxyErr(err))
	// The passed in context timed out. This is often due to the network being
	// down and the user hitting Ctrl-C.
	case <-ctx.Done():
		return nil, trace.ConnectionProblem(ctx.Err(), "connection canceled")
	}
}

// connectToProxy will dial to the proxy server and return a ProxyClient when
// successful.
func (tc *TeleportClient) connectToProxy(ctx context.Context) (*ProxyClient, error) {
	sshProxyAddr := tc.Config.SSHProxyAddr

	hostKeyCallback := tc.HostKeyCallback
	authMethods := append([]ssh.AuthMethod{}, tc.Config.AuthMethods...)
	clusterName := func() string { return tc.SiteName }
	if len(tc.JumpHosts) > 0 {
		log.Debugf("Overriding SSH proxy to JumpHosts's address %q", tc.JumpHosts[0].Addr.String())
		sshProxyAddr = tc.JumpHosts[0].Addr.Addr

		if tc.localAgent != nil {
			// Wrap host key and auth callbacks using clusterGuesser.
			//
			// clusterGuesser will use the host key callback to guess the target
			// cluster based on the host certificate. It will then use the auth
			// callback to load the appropriate SSH certificate for that cluster.
			clusterGuesser := newProxyClusterGuesser(hostKeyCallback, tc.SignersForClusterWithReissue)
			hostKeyCallback = clusterGuesser.hostKeyCallback
			authMethods = append(authMethods, clusterGuesser.authMethod(ctx))

			rootClusterName, err := tc.rootClusterName()
			if err != nil {
				return nil, trace.Wrap(err)
			}
			clusterName = func() string {
				// Only return the inferred cluster name if it's not the root
				// cluster. If it's the root cluster proxy, tc.SiteName could
				// be pointing at a leaf cluster and we don't want to override
				// that.
				if clusterGuesser.clusterName != rootClusterName {
					return clusterGuesser.clusterName
				}
				return tc.SiteName
			}
		}
	} else if tc.localAgent != nil {
		// Load SSH certs for all clusters we have, in case we don't yet
		// have a certificate for tc.SiteName (like during `tsh login leaf`).
		signers, err := tc.localAgent.Signers()
		// errNoLocalKeyStore is returned when running in the proxy. The proxy
		// should be passing auth methods via tc.Config.AuthMethods.
		if err != nil && !trace.IsNotFound(err) {
			return nil, trace.Wrap(err)
		}
		if len(signers) > 0 {
			authMethods = append(authMethods, ssh.PublicKeys(signers...))
		}
	}

	if len(authMethods) == 0 {
		return nil, trace.BadParameter("no SSH auth methods loaded, are you logged in?")
	}

	sshConfig := &ssh.ClientConfig{
		User:            tc.getProxySSHPrincipal(),
		HostKeyCallback: hostKeyCallback,
		Auth:            authMethods,
	}

	sshClient, err := makeProxySSHClient(ctx, tc, sshConfig)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	pc := &ProxyClient{
		teleportClient:  tc,
		Client:          sshClient,
		proxyAddress:    sshProxyAddr,
		proxyPrincipal:  sshConfig.User,
		hostKeyCallback: sshConfig.HostKeyCallback,
		authMethods:     sshConfig.Auth,
		hostLogin:       tc.HostLogin,
		siteName:        clusterName(),
		clientAddr:      tc.ClientAddr,
		Tracer:          tc.Tracer,
	}

	// Create the auth.ClientI for the local auth server
	// once per ProxyClient. This is an inexpensive
	// operation since the actual dialing of the auth
	// server is lazy - meaning it won't happen until the
	// first use of the auth.ClientI. By establishing
	// the auth.ClientI here we can ensure that any connections
	// to the local cluster will end up reusing this auth.ClientI
	// for the lifespan of the ProxyClient.
	clt, err := pc.ConnectToCluster(ctx, pc.siteName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	pc.currentCluster = clt

	return pc, nil
}

// makeProxySSHClient creates an SSH client by following steps:
//  1. If the current proxy supports TLS Routing and JumpHost address was not provided use TLSWrapper.
//  2. Check JumpHost raw SSH port or Teleport proxy address.
//     In case of proxy web address check if the proxy supports TLS Routing and connect to the proxy with TLSWrapper
//  3. Dial sshProxyAddr with raw SSH Dialer where sshProxyAddress is proxy ssh address or JumpHost address if
//     JumpHost address was provided.
func makeProxySSHClient(ctx context.Context, tc *TeleportClient, sshConfig *ssh.ClientConfig) (*tracessh.Client, error) {
	// Use TLS Routing dialer only if proxy support TLS Routing and JumpHost was not set.
	if tc.Config.TLSRoutingEnabled && len(tc.JumpHosts) == 0 {
		log.Infof("Connecting to proxy=%v login=%q using TLS Routing", tc.Config.WebProxyAddr, sshConfig.User)
		c, err := makeProxySSHClientWithTLSWrapper(ctx, tc, sshConfig, tc.Config.WebProxyAddr)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		log.Infof("Successful auth with proxy %v.", tc.Config.WebProxyAddr)
		return c, nil
	}

	sshProxyAddr := tc.Config.SSHProxyAddr

	// Handle situation where a Jump Host was set to proxy web address and Teleport supports TLS Routing.
	if len(tc.JumpHosts) > 0 {
		sshProxyAddr = tc.JumpHosts[0].Addr.Addr
		// Check if JumpHost address is a proxy web address.
		resp, err := webclient.Find(&webclient.Config{
			Context:      ctx,
			ProxyAddr:    sshProxyAddr,
			Insecure:     tc.InsecureSkipVerify,
			ExtraHeaders: tc.ExtraProxyHeaders,
		})
		// If JumpHost address is a proxy web port and proxy supports TLSRouting dial proxy with TLSWrapper.
		if err == nil && resp.Proxy.TLSRoutingEnabled {
			log.Infof("Connecting to proxy=%v login=%q using TLS Routing JumpHost", sshProxyAddr, sshConfig.User)
			c, err := makeProxySSHClientWithTLSWrapper(ctx, tc, sshConfig, sshProxyAddr)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			log.Infof("Successful auth with proxy %v.", sshProxyAddr)
			return c, nil
		}
	}

	log.Infof("Connecting to proxy=%v login=%q", sshProxyAddr, sshConfig.User)
	client, err := makeProxySSHClientDirect(ctx, tc, sshConfig, sshProxyAddr)
	if err != nil {
		if utils.IsHandshakeFailedError(err) {
			return nil, trace.AccessDenied("failed to authenticate with proxy %v: %v", sshProxyAddr, err)
		}

		return nil, trace.Wrap(err, "failed to authenticate with proxy %v", sshProxyAddr)
	}
	log.Infof("Successful auth with proxy %v.", sshProxyAddr)
	return client, nil
}

func makeProxySSHClientDirect(ctx context.Context, tc *TeleportClient, sshConfig *ssh.ClientConfig, proxyAddr string) (*tracessh.Client, error) {
	dialer := proxy.DialerFromEnvironment(tc.Config.SSHProxyAddr)
	return dialer.Dial(ctx, "tcp", proxyAddr, sshConfig)
}

func makeProxySSHClientWithTLSWrapper(ctx context.Context, tc *TeleportClient, sshConfig *ssh.ClientConfig, proxyAddr string) (*tracessh.Client, error) {
	tlsConfig, err := tc.LoadTLSConfig()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	tlsConfig.NextProtos = []string{string(alpncommon.ProtocolProxySSH)}
	dialer := proxy.DialerFromEnvironment(tc.Config.WebProxyAddr, proxy.WithALPNDialer(tlsConfig))
	return dialer.Dial(ctx, "tcp", proxyAddr, sshConfig)
}

func (tc *TeleportClient) rootClusterName() (string, error) {
	if tc.localAgent == nil {
		return "", trace.NotFound("cannot load root cluster name without local agent")
	}
	tlsKey, err := tc.localAgent.GetCoreKey()
	if err != nil {
		return "", trace.Wrap(err)
	}
	rootClusterName, err := tlsKey.RootClusterName()
	if err != nil {
		return "", trace.Wrap(err)
	}
	return rootClusterName, nil
}

// proxyClusterGuesser matches client SSH certificates to the target cluster of
// an SSH proxy. It uses an ssh.HostKeyCallback to infer the cluster name from
// the proxy host certificate. It then passes that name to signersForCluster to
// get the SSH certificates for that cluster.
type proxyClusterGuesser struct {
	clusterName string

	nextHostKeyCallback ssh.HostKeyCallback
	signersForCluster   func(context.Context, string) ([]ssh.Signer, error)
}

func newProxyClusterGuesser(nextHostKeyCallback ssh.HostKeyCallback, signersForCluster func(context.Context, string) ([]ssh.Signer, error)) *proxyClusterGuesser {
	return &proxyClusterGuesser{
		nextHostKeyCallback: nextHostKeyCallback,
		signersForCluster:   signersForCluster,
	}
}

func (g *proxyClusterGuesser) hostKeyCallback(hostname string, remote net.Addr, key ssh.PublicKey) error {
	cert, ok := key.(*ssh.Certificate)
	if !ok {
		return trace.BadParameter("remote proxy did not present a host certificate")
	}
	g.clusterName = cert.Permissions.Extensions[utils.CertExtensionAuthority]
	if g.clusterName == "" {
		log.Debugf("Target SSH server %q does not have a cluster name embedded in their certificate; will use all available client certificates to authenticate", hostname)
	}
	if g.nextHostKeyCallback != nil {
		return g.nextHostKeyCallback(hostname, remote, key)
	}
	return nil
}

func (g *proxyClusterGuesser) authMethod(ctx context.Context) ssh.AuthMethod {
	return ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
		return g.signersForCluster(ctx, g.clusterName)
	})
}

// WithoutJumpHosts executes the given function with a Teleport client that has
// no JumpHosts set, i.e. presumably falling back to the proxy specified in the
// profile.
func (tc *TeleportClient) WithoutJumpHosts(fn func(tcNoJump *TeleportClient) error) error {
	storedJumpHosts := tc.JumpHosts
	tc.JumpHosts = nil
	err := fn(tc)
	tc.JumpHosts = storedJumpHosts
	return trace.Wrap(err)
}

// Logout removes certificate and key for the currently logged in user from
// the filesystem and agent.
func (tc *TeleportClient) Logout() error {
	if tc.localAgent == nil {
		return nil
	}
	return tc.localAgent.DeleteKey()
}

// LogoutDatabase removes certificate for a particular database.
func (tc *TeleportClient) LogoutDatabase(dbName string) error {
	if tc.localAgent == nil {
		return nil
	}
	if tc.SiteName == "" {
		return trace.BadParameter("cluster name must be set for database logout")
	}
	if dbName == "" {
		return trace.BadParameter("please specify database name to log out of")
	}
	return tc.localAgent.DeleteUserCerts(tc.SiteName, WithDBCerts{dbName})
}

// LogoutApp removes certificate for the specified app.
func (tc *TeleportClient) LogoutApp(appName string) error {
	if tc.localAgent == nil {
		return nil
	}
	if tc.SiteName == "" {
		return trace.BadParameter("cluster name must be set for app logout")
	}
	if appName == "" {
		return trace.BadParameter("please specify app name to log out of")
	}
	return tc.localAgent.DeleteUserCerts(tc.SiteName, WithAppCerts{appName})
}

// LogoutAll removes all certificates for all users from the filesystem
// and agent.
func (tc *TeleportClient) LogoutAll() error {
	if tc.localAgent == nil {
		return nil
	}
	if err := tc.localAgent.DeleteKeys(); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// PingAndShowMOTD pings the Teleport Proxy and displays the Message Of The Day if it's available.
func (tc *TeleportClient) PingAndShowMOTD(ctx context.Context) (*webclient.PingResponse, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/PingAndShowMOTD",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	pr, err := tc.Ping(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if pr.Auth.HasMessageOfTheDay {
		err = tc.ShowMOTD(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	return pr, nil
}

// GetWebConfig retrieves Teleport proxy web config
func (tc *TeleportClient) GetWebConfig(ctx context.Context) (*webclient.WebConfig, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/GetWebConfig",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	cfg, err := GetWebConfig(ctx, tc.WebProxyAddr, tc.InsecureSkipVerify)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return cfg, nil
}

// Login logs the user into a Teleport cluster by talking to a Teleport proxy.
//
// The returned Key should typically be passed to ActivateKey in order to
// update local agent state.
//
// If the initial login fails due to a private key policy not being met, Login
// will automatically retry login with a private key that meets the required policy.
// This will initiate the same login flow again, aka prompt for password/otp/sso/mfa.
func (tc *TeleportClient) Login(ctx context.Context) (*Key, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/Login",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	// Ping the endpoint to see if it's up and find the type of authentication
	// supported, also show the message of the day if available.
	pr, err := tc.PingAndShowMOTD(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Get the SSHLoginFunc that matches client and cluster settings.
	sshLoginFunc, err := tc.getSSHLoginFunc(pr)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	key, err := tc.SSHLogin(ctx, sshLoginFunc)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Use proxy identity if set in key response.
	if key.Username != "" {
		tc.Username = key.Username
		if tc.localAgent != nil {
			tc.localAgent.username = key.Username
		}
	}

	return key, nil
}

// AttemptDeviceLogin attempts device authentication for the current device.
// It expects to receive the latest activated key, as acquired via
// [TeleportClient.Login], and augments the certificates within the key with
// device extensions.
//
// If successful, the new device certificates are automatically activated (using
// [TeleportClient.ActivateKey].)
//
// A nil response from this method doesn't mean that device authentication was
// successful, as skipping the ceremony is valid for various reasons (Teleport
// cluster doesn't support device authn, device wasn't enrolled, etc).
// Use [TeleportClient.DeviceLogin] if you want more control over process.
func (tc *TeleportClient) AttemptDeviceLogin(ctx context.Context, key *Key) error {
	pingResp, err := tc.Ping(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	if !tc.dtAttemptLoginIgnorePing && pingResp.Auth.DeviceTrustDisabled {
		log.Debug("Device Trust: skipping device authentication, device trust disabled")
		return nil
	}

	newCerts, err := tc.DeviceLogin(ctx, &devicepb.UserCertificates{
		// Augment the SSH certificate.
		// The TLS certificate is already part of the connection.
		SshAuthorizedKey: key.Cert,
	})
	if err != nil {
		log.WithError(err).Debug("Device Trust: device authentication failed")
		return nil // Swallowed on purpose.
	}

	log.Debug("Device Trust: acquired augmented user certificates")
	cp := *key
	cp.Cert = newCerts.SshAuthorizedKey
	cp.TLSCert = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: newCerts.X509Der,
	})
	return trace.Wrap(tc.ActivateKey(ctx, &cp))
}

// DeviceLogin attempts to authenticate the current device with Teleport.
// The device must be previously registered and enrolled for the authentication
// to succeed (see `tsh device enroll`).
//
// DeviceLogin may fail for a variety of reasons, some of them legitimate
// (non-Enterprise cluster, Device Trust is disabled, etc). Because of that, a
// failure in this method may not warrant failing a broader action (for example,
// `tsh login`).
//
// Device Trust is a Teleport Enterprise feature.
func (tc *TeleportClient) DeviceLogin(ctx context.Context, certs *devicepb.UserCertificates) (*devicepb.UserCertificates, error) {
	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	authClient, err := proxyClient.ConnectToRootCluster(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Allow tests to override the default authn function.
	runCeremony := tc.dtAuthnRunCeremony
	if runCeremony == nil {
		runCeremony = dtauthn.RunCeremony
	}

	newCerts, err := runCeremony(ctx, authClient.DevicesClient(), certs)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return newCerts, nil
}

// getSSHLoginFunc returns an SSHLoginFunc that matches client and cluster settings.
func (tc *TeleportClient) getSSHLoginFunc(pr *webclient.PingResponse) (SSHLoginFunc, error) {
	switch authType := pr.Auth.Type; {
	case authType == constants.Local && pr.Auth.Local != nil && pr.Auth.Local.Name == constants.PasswordlessConnector:
		// Sanity check settings.
		if !pr.Auth.AllowPasswordless {
			return nil, trace.BadParameter("passwordless disallowed by cluster settings")
		}
		return tc.pwdlessLogin, nil
	case authType == constants.Local && tc.canDefaultToPasswordless(pr):
		log.Debug("Trying passwordless login because credentials were found")
		// if passwordless is enabled and there are passwordless credentials
		// registered, we can try to go with passwordless login even though
		// auth=local was selected.
		return tc.pwdlessLogin, nil
	case authType == constants.Local:
		return func(ctx context.Context, priv *keys.PrivateKey) (*auth.SSHLoginResponse, error) {
			return tc.localLogin(ctx, priv, pr.Auth.SecondFactor)
		}, nil
	case authType == constants.OIDC:
		return func(ctx context.Context, priv *keys.PrivateKey) (*auth.SSHLoginResponse, error) {
			return tc.ssoLogin(ctx, priv, pr.Auth.OIDC.Name, constants.OIDC)
		}, nil
	case authType == constants.SAML:
		return func(ctx context.Context, priv *keys.PrivateKey) (*auth.SSHLoginResponse, error) {
			return tc.ssoLogin(ctx, priv, pr.Auth.SAML.Name, constants.SAML)
		}, nil
	case authType == constants.Github:
		return func(ctx context.Context, priv *keys.PrivateKey) (*auth.SSHLoginResponse, error) {
			return tc.ssoLogin(ctx, priv, pr.Auth.Github.Name, constants.Github)
		}, nil
	default:
		return nil, trace.BadParameter("unsupported authentication type: %q", pr.Auth.Type)
	}
}

// hasTouchIDCredentials provides indirection for tests.
var hasTouchIDCredentials = touchid.HasCredentials

// canDefaultToPasswordless checks without user interaction
// if there is any registered passwordless login.
func (tc *TeleportClient) canDefaultToPasswordless(pr *webclient.PingResponse) bool {
	// Verify if client flags are compatible with passwordless.
	allowedConnector := tc.AuthConnector == ""
	allowedAttachment := tc.AuthenticatorAttachment == wancli.AttachmentAuto || tc.AuthenticatorAttachment == wancli.AttachmentPlatform
	if !allowedConnector || !allowedAttachment || tc.PreferOTP {
		return false
	}

	// Verify if server is compatible with passwordless.
	if !pr.Auth.AllowPasswordless || pr.Auth.Webauthn == nil {
		return false
	}

	// Only pass on the user if explicitly set, otherwise let the credential
	// picker kick in.
	user := ""
	if tc.ExplicitUsername {
		user = tc.Username
	}

	return hasTouchIDCredentials(pr.Auth.Webauthn.RPID, user)
}

// SSHLoginFunc is a function which carries out authn with an auth server and returns an auth response.
type SSHLoginFunc func(context.Context, *keys.PrivateKey) (*auth.SSHLoginResponse, error)

// SSHLogin uses the given login function to login the client. This function handles
// private key logic and parsing the resulting auth response.
func (tc *TeleportClient) SSHLogin(ctx context.Context, sshLoginFunc SSHLoginFunc) (*Key, error) {
	priv, err := tc.GetNewLoginKey(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	response, err := sshLoginFunc(ctx, priv)
	if err != nil {
		// check if the error is a private key policy error, and relogin if it is.
		if privateKeyPolicy, parseErr := keys.ParsePrivateKeyPolicyError(err); parseErr == nil {
			// The current private key was rejected due to an unmet key policy requirement.
			fmt.Fprintf(tc.Stderr, "Unmet private key policy %q.\n", privateKeyPolicy)

			// Set the private key policy to the expected value and re-login.
			tc.PrivateKeyPolicy = privateKeyPolicy
			priv, err = tc.GetNewLoginKey(ctx)
			if err != nil {
				return nil, trace.Wrap(err)
			}

			fmt.Fprintf(tc.Stderr, "Re-initiating login with YubiKey generated private key.\n")
			response, err = sshLoginFunc(ctx, priv)
		}
	}

	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Check that a host certificate for at least one cluster was returned.
	if len(response.HostSigners) == 0 {
		return nil, trace.BadParameter("bad response from the server: expected at least one certificate, got 0")
	}

	// extract the new certificate out of the response
	key := NewKey(priv)
	key.Cert = response.Cert
	key.TLSCert = response.TLSCert
	key.TrustedCerts = response.HostSigners
	key.Username = response.Username
	key.ProxyHost = tc.WebProxyHost()

	if tc.KubernetesCluster != "" {
		key.KubeTLSCerts[tc.KubernetesCluster] = response.TLSCert
	}
	if tc.DatabaseService != "" {
		key.DBTLSCerts[tc.DatabaseService] = response.TLSCert
	}

	// Store the requested cluster name in the key.
	key.ClusterName = tc.SiteName
	if key.ClusterName == "" {
		rootClusterName := key.TrustedCerts[0].ClusterName
		key.ClusterName = rootClusterName
		tc.SiteName = rootClusterName
	}

	return key, nil
}

// GetNewLoginKey gets a new private key for login.
func (tc *TeleportClient) GetNewLoginKey(ctx context.Context) (priv *keys.PrivateKey, err error) {
	switch tc.PrivateKeyPolicy {
	case keys.PrivateKeyPolicyHardwareKey:
		log.Debugf("Attempting to login with YubiKey private key.")
		priv, err = keys.GetOrGenerateYubiKeyPrivateKey(false)
	case keys.PrivateKeyPolicyHardwareKeyTouch:
		log.Debugf("Attempting to login with YubiKey private key with touch required.")
		priv, err = keys.GetOrGenerateYubiKeyPrivateKey(true)
	default:
		log.Debugf("Attempting to login with a new RSA private key.")
		priv, err = native.GeneratePrivateKey()
	}

	if err != nil {
		return nil, trace.Wrap(err)
	}
	return priv, nil
}

// new SSHLogin generates a new SSHLogin using the given login key.
func (tc *TeleportClient) newSSHLogin(priv *keys.PrivateKey) (SSHLogin, error) {
	attestationStatement, err := keys.GetAttestationStatement(priv)
	if err != nil {
		return SSHLogin{}, trace.Wrap(err)
	}

	return SSHLogin{
		ProxyAddr:            tc.WebProxyAddr,
		PubKey:               priv.MarshalSSHPublicKey(),
		TTL:                  tc.KeyTTL,
		Insecure:             tc.InsecureSkipVerify,
		Pool:                 loopbackPool(tc.WebProxyAddr),
		Compatibility:        tc.CertificateFormat,
		RouteToCluster:       tc.SiteName,
		KubernetesCluster:    tc.KubernetesCluster,
		AttestationStatement: attestationStatement,
		ExtraHeaders:         tc.ExtraProxyHeaders,
	}, nil
}

func (tc *TeleportClient) pwdlessLogin(ctx context.Context, priv *keys.PrivateKey) (*auth.SSHLoginResponse, error) {
	// Only pass on the user if explicitly set, otherwise let the credential
	// picker kick in.
	user := ""
	if tc.ExplicitUsername {
		user = tc.Username
	}

	sshLogin, err := tc.newSSHLogin(priv)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	response, err := SSHAgentPasswordlessLogin(ctx, SSHLoginPasswordless{
		SSHLogin:                sshLogin,
		User:                    user,
		AuthenticatorAttachment: tc.AuthenticatorAttachment,
		StderrOverride:          tc.Stderr,
	})

	return response, trace.Wrap(err)
}

func (tc *TeleportClient) localLogin(ctx context.Context, priv *keys.PrivateKey, secondFactor constants.SecondFactorType) (*auth.SSHLoginResponse, error) {
	var err error
	var response *auth.SSHLoginResponse

	// TODO(awly): mfa: ideally, clients should always go through mfaLocalLogin
	// (with a nop MFA challenge if no 2nd factor is required). That way we can
	// deprecate the direct login endpoint.
	switch secondFactor {
	case constants.SecondFactorOff, constants.SecondFactorOTP:
		response, err = tc.directLogin(ctx, secondFactor, priv)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	case constants.SecondFactorU2F, constants.SecondFactorWebauthn, constants.SecondFactorOn, constants.SecondFactorOptional:
		response, err = tc.mfaLocalLogin(ctx, priv)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	default:
		return nil, trace.BadParameter("unsupported second factor type: %q", secondFactor)
	}

	// Ignore username returned from proxy
	response.Username = ""
	return response, nil
}

// directLogin asks for a password + OTP token, makes a request to CA via proxy
func (tc *TeleportClient) directLogin(ctx context.Context, secondFactorType constants.SecondFactorType, priv *keys.PrivateKey) (*auth.SSHLoginResponse, error) {
	password, err := tc.AskPassword(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Only ask for a second factor if it's enabled.
	var otpToken string
	if secondFactorType == constants.SecondFactorOTP {
		otpToken, err = tc.AskOTP(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	sshLogin, err := tc.newSSHLogin(priv)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Ask the CA (via proxy) to sign our public key:
	response, err := SSHAgentLogin(ctx, SSHLoginDirect{
		SSHLogin: sshLogin,
		User:     tc.Username,
		Password: password,
		OTPToken: otpToken,
	})

	return response, trace.Wrap(err)
}

// mfaLocalLogin asks for a password and performs the challenge-response authentication
func (tc *TeleportClient) mfaLocalLogin(ctx context.Context, priv *keys.PrivateKey) (*auth.SSHLoginResponse, error) {
	password, err := tc.AskPassword(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	sshLogin, err := tc.newSSHLogin(priv)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	response, err := SSHAgentMFALogin(ctx, SSHLoginMFA{
		SSHLogin:                sshLogin,
		User:                    tc.Username,
		Password:                password,
		AuthenticatorAttachment: tc.AuthenticatorAttachment,
		PreferOTP:               tc.PreferOTP,
		AllowStdinHijack:        tc.AllowStdinHijack,
	})

	return response, trace.Wrap(err)
}

// SSOLoginFunc is a function used in tests to mock SSO logins.
type SSOLoginFunc func(ctx context.Context, connectorID string, priv *keys.PrivateKey, protocol string) (*auth.SSHLoginResponse, error)

// samlLogin opens browser window and uses OIDC or SAML redirect cycle with browser
func (tc *TeleportClient) ssoLogin(ctx context.Context, priv *keys.PrivateKey, connectorID string, protocol string) (*auth.SSHLoginResponse, error) {
	if tc.MockSSOLogin != nil {
		// sso login response is being mocked for testing purposes
		return tc.MockSSOLogin(ctx, connectorID, priv, protocol)
	}

	sshLogin, err := tc.newSSHLogin(priv)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// ask the CA (via proxy) to sign our public key:
	response, err := SSHAgentSSOLogin(ctx, SSHLoginSSO{
		SSHLogin:    sshLogin,
		ConnectorID: connectorID,
		Protocol:    protocol,
		BindAddr:    tc.BindAddr,
		Browser:     tc.Browser,
	}, nil)
	return response, trace.Wrap(err)
}

// ActivateKey saves the target session cert into the local
// keystore (and into the ssh-agent) for future use.
func (tc *TeleportClient) ActivateKey(ctx context.Context, key *Key) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/ActivateKey",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	if tc.localAgent == nil {
		// skip activation if no local agent is present
		return nil
	}

	// save the cert to the local storage (~/.tsh usually):
	if err := tc.localAgent.AddKey(key); err != nil {
		return trace.Wrap(err)
	}

	// Connect to the Auth Server of the root cluster and fetch the known hosts.
	rootClusterName := key.TrustedCerts[0].ClusterName
	if err := tc.UpdateTrustedCA(ctx, rootClusterName); err != nil {
		if len(tc.JumpHosts) == 0 {
			return trace.Wrap(err)
		}
		errViaJumphost := err
		// If JumpHosts was pointing at the leaf cluster (e.g. during 'tsh ssh
		// -J leaf.example.com'), this could've caused the above error. Try to
		// fetch CAs without JumpHosts to force it to use the root cluster.
		if err := tc.WithoutJumpHosts(func(tc *TeleportClient) error {
			return tc.UpdateTrustedCA(ctx, rootClusterName)
		}); err != nil {
			return trace.NewAggregate(errViaJumphost, err)
		}
	}

	return nil
}

// Ping makes a ping request to the proxy, and updates tc based on the
// response. The successful ping response is cached, multiple calls to Ping
// will return the original response and skip the round-trip.
//
// Ping can be called for its side-effect of applying the proxy-provided
// settings (such as various listening addresses).
func (tc *TeleportClient) Ping(ctx context.Context) (*webclient.PingResponse, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/Ping",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	// If, at some point, there's a need to bypass this caching, consider
	// adding a bool argument. At the time of writing this we always want to
	// cache.
	if tc.lastPing != nil {
		return tc.lastPing, nil
	}
	pr, err := webclient.Ping(&webclient.Config{
		Context:       ctx,
		ProxyAddr:     tc.WebProxyAddr,
		Insecure:      tc.InsecureSkipVerify,
		Pool:          loopbackPool(tc.WebProxyAddr),
		ConnectorName: tc.AuthConnector,
		ExtraHeaders:  tc.ExtraProxyHeaders,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// If version checking was requested and the server advertises a minimum version.
	if tc.CheckVersions && pr.MinClientVersion != "" {
		if err := utils.CheckVersion(teleport.Version, pr.MinClientVersion); err != nil && trace.IsBadParameter(err) {
			fmt.Fprintf(tc.Config.Stderr, `
			WARNING
			Detected potentially incompatible client and server versions.
			Minimum client version supported by the server is %v but you are using %v.
			Please upgrade tsh to %v or newer or use the --skip-version-check flag to bypass this check.
			Future versions of tsh will fail when incompatible versions are detected.
			`, pr.MinClientVersion, teleport.Version, pr.MinClientVersion)
		}
	}

	// Update tc with proxy and auth settings specified in Ping response.
	if err := tc.applyProxySettings(pr.Proxy); err != nil {
		return nil, trace.Wrap(err)
	}
	tc.applyAuthSettings(pr.Auth)

	tc.lastPing = pr

	return pr, nil
}

// ShowMOTD fetches the cluster MotD, displays it (if any) and waits for
// confirmation from the user.
func (tc *TeleportClient) ShowMOTD(ctx context.Context) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/ShowMOTD",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	motd, err := webclient.GetMOTD(
		&webclient.Config{
			Context:      ctx,
			ProxyAddr:    tc.WebProxyAddr,
			Insecure:     tc.InsecureSkipVerify,
			Pool:         loopbackPool(tc.WebProxyAddr),
			ExtraHeaders: tc.ExtraProxyHeaders,
		})
	if err != nil {
		return trace.Wrap(err)
	}

	if motd.Text != "" {
		fmt.Fprintf(tc.Config.Stderr, "%s\nPress [ENTER] to continue.\n", motd.Text)
		// We're re-using the password reader for user acknowledgment for
		// aesthetic purposes, because we want to hide any garbage the
		// use might enter at the prompt. Whatever the user enters will
		// be simply discarded, and the user can still CTRL+C out if they
		// disagree.
		_, err := prompt.Stdin().ReadPassword(context.Background())
		if err != nil {
			return trace.Wrap(err)
		}
	}

	return nil
}

// GetTrustedCA returns a list of host certificate authorities
// trusted by the cluster client is authenticated with.
func (tc *TeleportClient) GetTrustedCA(ctx context.Context, clusterName string) ([]types.CertAuthority, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/GetTrustedCA",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(attribute.String("cluster", clusterName)),
	)
	defer span.End()

	// Connect to the proxy.
	if !tc.Config.ProxySpecified() {
		return nil, trace.BadParameter("proxy server is not specified")
	}
	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()

	// Get a client to the Auth Server.
	clt, err := proxyClient.ConnectToCluster(ctx, clusterName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Get the list of host certificates that this cluster knows about.
	return clt.GetCertAuthorities(ctx, types.HostCA, false)
}

// UpdateTrustedCA connects to the Auth Server and fetches all host certificates
// and updates ~/.tsh/keys/proxy/certs.pem and ~/.tsh/known_hosts.
func (tc *TeleportClient) UpdateTrustedCA(ctx context.Context, clusterName string) error {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/UpdateTrustedCA",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(attribute.String("cluster", clusterName)),
	)
	defer span.End()

	if tc.localAgent == nil {
		return trace.BadParameter("TeleportClient.UpdateTrustedCA called on a client without localAgent")
	}
	// Get the list of host certificates that this cluster knows about.
	hostCerts, err := tc.GetTrustedCA(ctx, clusterName)
	if err != nil {
		return trace.Wrap(err)
	}
	trustedCerts := auth.AuthoritiesToTrustedCerts(hostCerts)

	// Update the CA pool and known hosts for all CAs the cluster knows about.
	err = tc.localAgent.SaveTrustedCerts(trustedCerts)
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

// applyProxySettings updates configuration changes based on the advertised
// proxy settings, overriding existing fields in tc.
func (tc *TeleportClient) applyProxySettings(proxySettings webclient.ProxySettings) error {
	// Kubernetes proxy settings.
	if proxySettings.Kube.Enabled {
		switch {
		// PublicAddr is the first preference.
		case proxySettings.Kube.PublicAddr != "":
			if _, err := utils.ParseAddr(proxySettings.Kube.PublicAddr); err != nil {
				return trace.BadParameter(
					"failed to parse value received from the server: %q, contact your administrator for help",
					proxySettings.Kube.PublicAddr)
			}
			tc.KubeProxyAddr = proxySettings.Kube.PublicAddr
		// ListenAddr is the second preference.
		case proxySettings.Kube.ListenAddr != "":
			addr, err := utils.ParseAddr(proxySettings.Kube.ListenAddr)
			if err != nil {
				return trace.BadParameter(
					"failed to parse value received from the server: %q, contact your administrator for help",
					proxySettings.Kube.ListenAddr)
			}
			// If ListenAddr host is 0.0.0.0 or [::], replace it with something
			// routable from the web endpoint.
			if net.ParseIP(addr.Host()).IsUnspecified() {
				webProxyHost, _ := tc.WebProxyHostPort()
				tc.KubeProxyAddr = net.JoinHostPort(webProxyHost, strconv.Itoa(addr.Port(defaults.KubeListenPort)))
			} else {
				tc.KubeProxyAddr = proxySettings.Kube.ListenAddr
			}
		// If neither PublicAddr nor TunnelAddr are passed, use the web
		// interface hostname with default k8s port as a guess.
		default:
			webProxyHost, _ := tc.WebProxyHostPort()
			tc.KubeProxyAddr = net.JoinHostPort(webProxyHost, strconv.Itoa(defaults.KubeListenPort))
		}
	} else {
		// Zero the field, in case there was a previous value set (e.g. loaded
		// from profile directory).
		tc.KubeProxyAddr = ""
	}

	// Read in settings for HTTP endpoint of the proxy.
	if proxySettings.SSH.PublicAddr != "" {
		addr, err := utils.ParseAddr(proxySettings.SSH.PublicAddr)
		if err != nil {
			return trace.BadParameter(
				"failed to parse value received from the server: %q, contact your administrator for help",
				proxySettings.SSH.PublicAddr)
		}
		tc.WebProxyAddr = net.JoinHostPort(addr.Host(), strconv.Itoa(addr.Port(defaults.HTTPListenPort)))

		if tc.localAgent != nil {
			// Update local agent (that reads/writes to ~/.tsh) with the new address
			// of the web proxy. This will control where the keys are stored on disk
			// after login.
			tc.localAgent.UpdateProxyHost(addr.Host())
		}
	}
	// Read in settings for the SSH endpoint of the proxy.
	//
	// If listen_addr is set, take host from ProxyWebHost and port from what
	// was set. This is to maintain backward compatibility when Teleport only
	// supported public_addr.
	if proxySettings.SSH.ListenAddr != "" {
		addr, err := utils.ParseAddr(proxySettings.SSH.ListenAddr)
		if err != nil {
			return trace.BadParameter(
				"failed to parse value received from the server: %q, contact your administrator for help",
				proxySettings.SSH.ListenAddr)
		}
		webProxyHost, _ := tc.WebProxyHostPort()
		tc.SSHProxyAddr = net.JoinHostPort(webProxyHost, strconv.Itoa(addr.Port(defaults.SSHProxyListenPort)))
	}
	// If ssh_public_addr is set, override settings from listen_addr.
	if proxySettings.SSH.SSHPublicAddr != "" {
		addr, err := utils.ParseAddr(proxySettings.SSH.SSHPublicAddr)
		if err != nil {
			return trace.BadParameter(
				"failed to parse value received from the server: %q, contact your administrator for help",
				proxySettings.SSH.SSHPublicAddr)
		}
		tc.SSHProxyAddr = net.JoinHostPort(addr.Host(), strconv.Itoa(addr.Port(defaults.SSHProxyListenPort)))
	}

	// Read Postgres proxy settings.
	switch {
	case proxySettings.DB.PostgresPublicAddr != "":
		addr, err := utils.ParseAddr(proxySettings.DB.PostgresPublicAddr)
		if err != nil {
			return trace.BadParameter("failed to parse Postgres public address received from server: %q, contact your administrator for help",
				proxySettings.DB.PostgresPublicAddr)
		}
		tc.PostgresProxyAddr = net.JoinHostPort(addr.Host(), strconv.Itoa(addr.Port(tc.WebProxyPort())))
	case proxySettings.DB.PostgresListenAddr != "":
		addr, err := utils.ParseAddr(proxySettings.DB.PostgresListenAddr)
		if err != nil {
			return trace.BadParameter("failed to parse Postgres listen address received from server: %q, contact your administrator for help",
				proxySettings.DB.PostgresListenAddr)
		}
		tc.PostgresProxyAddr = net.JoinHostPort(tc.WebProxyHost(), strconv.Itoa(addr.Port(defaults.PostgresListenPort)))
	default:
		webProxyHost, webProxyPort := tc.WebProxyHostPort()
		tc.PostgresProxyAddr = net.JoinHostPort(webProxyHost, strconv.Itoa(webProxyPort))
	}

	// Read Mongo proxy settings.
	switch {
	case proxySettings.DB.MongoPublicAddr != "":
		addr, err := utils.ParseAddr(proxySettings.DB.MongoPublicAddr)
		if err != nil {
			return trace.BadParameter("failed to parse Mongo public address received from server: %q, contact your administrator for help",
				proxySettings.DB.MongoPublicAddr)
		}
		tc.MongoProxyAddr = net.JoinHostPort(addr.Host(), strconv.Itoa(addr.Port(tc.WebProxyPort())))
	case proxySettings.DB.MongoListenAddr != "":
		addr, err := utils.ParseAddr(proxySettings.DB.MongoListenAddr)
		if err != nil {
			return trace.BadParameter("failed to parse Mongo listen address received from server: %q, contact your administrator for help",
				proxySettings.DB.MongoListenAddr)
		}
		tc.MongoProxyAddr = net.JoinHostPort(tc.WebProxyHost(), strconv.Itoa(addr.Port(defaults.MongoListenPort)))
	}

	// Read MySQL proxy settings if enabled on the server.
	switch {
	case proxySettings.DB.MySQLPublicAddr != "":
		addr, err := utils.ParseAddr(proxySettings.DB.MySQLPublicAddr)
		if err != nil {
			return trace.BadParameter("failed to parse MySQL public address received from server: %q, contact your administrator for help",
				proxySettings.DB.MySQLPublicAddr)
		}
		tc.MySQLProxyAddr = net.JoinHostPort(addr.Host(), strconv.Itoa(addr.Port(defaults.MySQLListenPort)))
	case proxySettings.DB.MySQLListenAddr != "":
		addr, err := utils.ParseAddr(proxySettings.DB.MySQLListenAddr)
		if err != nil {
			return trace.BadParameter("failed to parse MySQL listen address received from server: %q, contact your administrator for help",
				proxySettings.DB.MySQLListenAddr)
		}
		tc.MySQLProxyAddr = net.JoinHostPort(tc.WebProxyHost(), strconv.Itoa(addr.Port(defaults.MySQLListenPort)))
	}

	tc.TLSRoutingEnabled = proxySettings.TLSRoutingEnabled
	if tc.TLSRoutingEnabled {
		// If proxy supports TLS Routing all k8s requests will be sent to the WebProxyAddr where TLS Routing will identify
		// k8s requests by "kube." SNI prefix and route to the kube proxy service.
		tc.KubeProxyAddr = tc.WebProxyAddr
	}

	return nil
}

// applyAuthSettings updates configuration changes based on the advertised
// authentication settings, overriding existing fields in tc.
func (tc *TeleportClient) applyAuthSettings(authSettings webclient.AuthenticationSettings) {
	tc.LoadAllCAs = authSettings.LoadAllCAs

	// Update the private key policy from auth settings if it is stricter than the saved setting.
	if authSettings.PrivateKeyPolicy != "" && authSettings.PrivateKeyPolicy.VerifyPolicy(tc.PrivateKeyPolicy) != nil {
		tc.PrivateKeyPolicy = authSettings.PrivateKeyPolicy
	}
}

// AddTrustedCA adds a new CA as trusted CA for this client, used in tests
func (tc *TeleportClient) AddTrustedCA(ctx context.Context, ca types.CertAuthority) error {
	_, span := tc.Tracer.Start(
		ctx,
		"teleportClient/AddTrustedCA",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)
	defer span.End()

	if tc.localAgent == nil {
		return trace.BadParameter("TeleportClient.AddTrustedCA called on a client without localAgent")
	}

	err := tc.localAgent.SaveTrustedCerts(auth.AuthoritiesToTrustedCerts([]types.CertAuthority{ca}))
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

// AddKey adds a key to the client's local agent, used in tests.
func (tc *TeleportClient) AddKey(key *Key) error {
	if tc.localAgent == nil {
		return trace.BadParameter("TeleportClient.AddKey called on a client without localAgent")
	}
	if key.ClusterName == "" {
		key.ClusterName = tc.SiteName
	}
	return tc.localAgent.AddKey(key)
}

// SendEvent adds a events.EventFields to the channel.
func (tc *TeleportClient) SendEvent(ctx context.Context, e events.EventFields) error {
	// Try and send the event to the eventsCh. If blocking, keep blocking until
	// the passed in context in canceled.
	select {
	case tc.eventsCh <- e:
		return nil
	case <-ctx.Done():
		return trace.Wrap(ctx.Err())
	}
}

// EventsChannel returns a channel that can be used to listen for events that
// occur for this session.
func (tc *TeleportClient) EventsChannel() <-chan events.EventFields {
	return tc.eventsCh
}

// loopbackPool reads trusted CAs if it finds it in a predefined location
// and will work only if target proxy address is loopback
func loopbackPool(proxyAddr string) *x509.CertPool {
	if !apiutils.IsLoopback(proxyAddr) {
		log.Debugf("not using loopback pool for remote proxy addr: %v", proxyAddr)
		return nil
	}
	log.Debugf("attempting to use loopback pool for local proxy addr: %v", proxyAddr)
	certPool, err := x509.SystemCertPool()
	if err != nil {
		log.Debugf("could not open system cert pool, using empty cert pool instead: %v", err)
		certPool = x509.NewCertPool()
	}

	certPath := filepath.Join(defaults.DataDir, defaults.SelfSignedCertPath)
	log.Debugf("reading self-signed certs from: %v", certPath)

	pemByte, err := os.ReadFile(certPath)
	if err != nil {
		log.Debugf("could not open any path in: %v", certPath)
		return nil
	}

	for {
		var block *pem.Block
		block, pemByte = pem.Decode(pemByte)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			log.Debugf("could not parse cert in: %v, err: %v", certPath, err)
			return nil
		}
		certPool.AddCert(cert)
	}
	log.Debugf("using local pool for loopback proxy: %v, err: %v", certPath, err)
	return certPool
}

// connectToSSHAgent connects to the system SSH agent and returns an agent.Agent.
func connectToSSHAgent() agent.ExtendedAgent {
	socketPath := os.Getenv(teleport.SSHAuthSock)
	conn, err := agentconn.Dial(socketPath)
	if err != nil {
		log.Errorf("[KEY AGENT] Unable to connect to SSH agent on socket: %q.", socketPath)
		return nil
	}

	log.Infof("[KEY AGENT] Connected to the system agent: %q", socketPath)
	return agent.NewClient(conn)
}

// Username returns the current user's username
func Username() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", trace.Wrap(err)
	}

	username := u.Username

	// If on Windows, strip the domain name.
	if runtime.GOOS == constants.WindowsOS {
		idx := strings.LastIndex(username, "\\")
		if idx > -1 {
			username = username[idx+1:]
		}
	}

	return username, nil
}

// AskOTP prompts the user to enter the OTP token.
func (tc *TeleportClient) AskOTP(ctx context.Context) (token string, err error) {
	return prompt.Password(ctx, tc.Stderr, prompt.Stdin(), "Enter your OTP token")
}

// AskPassword prompts the user to enter the password
func (tc *TeleportClient) AskPassword(ctx context.Context) (pwd string, err error) {
	return prompt.Password(
		ctx, tc.Stderr, prompt.Stdin(), fmt.Sprintf("Enter password for Teleport user %v", tc.Config.Username))
}

// LoadTLSConfig returns the user's TLS configuration for an external identity if the SkipLocalAuth flag was set
// or teleport core TLS certificate for the local agent.
func (tc *TeleportClient) LoadTLSConfig() (*tls.Config, error) {
	// if SkipLocalAuth flag is set use an external identity file instead of loading cert from the local agent.
	if tc.TLS != nil {
		return tc.TLS.Clone(), nil
	}

	tlsKey, err := tc.localAgent.GetCoreKey()
	if err != nil {
		return nil, trace.Wrap(err, "failed to fetch TLS key for %v", tc.Username)
	}

	var clusters []string
	if tc.LoadAllCAs {
		clusters, err = tc.localAgent.GetClusterNames()
		if err != nil {
			return nil, trace.Wrap(err)
		}
	} else {
		rootCluster, err := tlsKey.RootClusterName()
		if err != nil {
			return nil, trace.Wrap(err)
		}
		clusters = []string{rootCluster}
		if tc.SiteName != "" && rootCluster != tc.SiteName {
			// In case of establishing connection to leaf cluster the client validate
			// ssh cert against root cluster proxy cert and leaf cluster cert.
			clusters = append(clusters, tc.SiteName)
		}
	}

	tlsConfig, err := tlsKey.TeleportClientTLSConfig(nil, clusters)
	if err != nil {
		return nil, trace.Wrap(err, "failed to generate client TLS config")
	}
	return tlsConfig, nil
}

// ParseLabelSpec parses a string like 'name=value,"long name"="quoted value"` into a map like
// { "name" -> "value", "long name" -> "quoted value" }
func ParseLabelSpec(spec string) (map[string]string, error) {
	var tokens []string
	openQuotes := false
	var tokenStart, assignCount int
	specLen := len(spec)
	// tokenize the label spec:
	for i, ch := range spec {
		endOfToken := false
		// end of line?
		if i+utf8.RuneLen(ch) == specLen {
			i += utf8.RuneLen(ch)
			endOfToken = true
		}
		switch ch {
		case '"':
			openQuotes = !openQuotes
		case '=', ',', ';':
			if !openQuotes {
				endOfToken = true
				if ch == '=' {
					assignCount++
				}
			}
		}
		if endOfToken && i > tokenStart {
			tokens = append(tokens, strings.TrimSpace(strings.Trim(spec[tokenStart:i], `"`)))
			tokenStart = i + 1
		}
	}
	// simple validation of tokenization: must have an even number of tokens (because they're pairs)
	// and the number of such pairs must be equal the number of assignments
	if len(tokens)%2 != 0 || assignCount != len(tokens)/2 {
		return nil, fmt.Errorf("invalid label spec: '%s', should be 'key=value'", spec)
	}
	// break tokens in pairs and put into a map:
	labels := make(map[string]string)
	for i := 0; i < len(tokens); i += 2 {
		labels[tokens[i]] = tokens[i+1]
	}
	return labels, nil
}

// ParseSearchKeywords parses a string ie: foo,bar,"quoted value"` into a slice of
// strings: ["foo", "bar", "quoted value"].
// Almost a replica to ParseLabelSpec, but with few modifications such as
// allowing a custom delimiter. Defaults to comma delimiter if not defined.
func ParseSearchKeywords(spec string, customDelimiter rune) []string {
	delimiter := customDelimiter
	if delimiter == 0 {
		delimiter = rune(',')
	}

	var tokens []string
	openQuotes := false
	var tokenStart int
	specLen := len(spec)
	// tokenize the label search:
	for i, ch := range spec {
		endOfToken := false
		if i+utf8.RuneLen(ch) == specLen {
			i += utf8.RuneLen(ch)
			endOfToken = true
		}
		switch ch {
		case '"':
			openQuotes = !openQuotes
		case delimiter:
			if !openQuotes {
				endOfToken = true
			}
		}
		if endOfToken && i > tokenStart {
			tokens = append(tokens, strings.TrimSpace(strings.Trim(spec[tokenStart:i], `"`)))
			tokenStart = i + 1
		}
	}

	return tokens
}

// Executes the given command on the client machine (localhost). If no command is given,
// executes shell
func runLocalCommand(command []string) error {
	if len(command) == 0 {
		user, err := user.Current()
		if err != nil {
			return trace.Wrap(err)
		}
		shell, err := shell.GetLoginShell(user.Username)
		if err != nil {
			return trace.Wrap(err)
		}
		command = []string{shell}
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	return cmd.Run()
}

// String returns the same string spec which can be parsed by ParsePortForwardSpec.
func (fp ForwardedPorts) String() (retval []string) {
	for _, p := range fp {
		retval = append(retval, p.ToString())
	}
	return retval
}

// ParsePortForwardSpec parses parameter to -L flag, i.e. strings like "[ip]:80:remote.host:3000"
// The opposite of this function (spec generation) is ForwardedPorts.String()
func ParsePortForwardSpec(spec []string) (ports ForwardedPorts, err error) {
	if len(spec) == 0 {
		return ports, nil
	}
	const errTemplate = "invalid port forwarding spec '%s': expected format `80:remote.host:80`"
	ports = make([]ForwardedPort, len(spec))

	for i, str := range spec {
		parts := strings.Split(str, ":")
		if len(parts) < 3 || len(parts) > 4 {
			return nil, trace.BadParameter(errTemplate, str)
		}
		if len(parts) == 3 {
			parts = append([]string{"127.0.0.1"}, parts...)
		}
		p := &ports[i]
		p.SrcIP = parts[0]
		p.SrcPort, err = strconv.Atoi(parts[1])
		if err != nil {
			return nil, trace.BadParameter(errTemplate, str)
		}
		p.DestHost = parts[2]
		p.DestPort, err = strconv.Atoi(parts[3])
		if err != nil {
			return nil, trace.BadParameter(errTemplate, str)
		}
	}
	return ports, nil
}

// String returns the same string spec which can be parsed by
// ParseDynamicPortForwardSpec.
func (fp DynamicForwardedPorts) String() (retval []string) {
	for _, p := range fp {
		retval = append(retval, p.ToString())
	}
	return retval
}

// ParseDynamicPortForwardSpec parses the dynamic port forwarding spec
// passed in the -D flag. The format of the dynamic port forwarding spec
// is [bind_address:]port.
func ParseDynamicPortForwardSpec(spec []string) (DynamicForwardedPorts, error) {
	result := make(DynamicForwardedPorts, 0, len(spec))

	for _, str := range spec {
		// Check whether this is only the port number, like "1080".
		// net.SplitHostPort would fail on that unless there's a colon in
		// front.
		if !strings.Contains(str, ":") {
			str = ":" + str
		}
		host, port, err := net.SplitHostPort(str)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		// If no host is provided, bind to localhost.
		if host == "" {
			host = defaults.Localhost
		}

		srcPort, err := strconv.Atoi(port)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		result = append(result, DynamicForwardedPort{
			SrcIP:   host,
			SrcPort: srcPort,
		})
	}

	return result, nil
}

// InsecureSkipHostKeyChecking is used when the user passes in
// "StrictHostKeyChecking yes".
func InsecureSkipHostKeyChecking(host string, remote net.Addr, key ssh.PublicKey) error {
	return nil
}

// isFIPS returns if the binary was build with BoringCrypto, which implies
// FedRAMP/FIPS 140-2 mode for tsh.
func isFIPS() bool {
	return modules.GetModules().IsBoringBinary()
}

// playSession plays session in the terminal
func playSession(sessionEvents []events.EventFields, stream []byte) error {
	term, err := terminal.New(nil, nil, nil)
	if err != nil {
		return trace.Wrap(err)
	}

	defer term.Close()

	// configure terminal for direct unbuffered echo-less input:
	if term.IsAttached() {
		err := term.InitRaw(true)
		if err != nil {
			return trace.Wrap(err)
		}
	}

	player := newSessionPlayer(sessionEvents, stream, term)
	errorCh := make(chan error)
	// keys:
	const (
		keyCtrlC = 3
		keyCtrlD = 4
		keySpace = 32
		keyLeft  = 68
		keyRight = 67
		keyUp    = 65
		keyDown  = 66
	)
	// playback control goroutine
	go func() {
		defer player.EndPlayback()
		var key [1]byte
		for {
			_, err := term.Stdin().Read(key[:])
			if err != nil {
				errorCh <- err
				return
			}
			switch key[0] {
			// Ctrl+C or Ctrl+D
			case keyCtrlC, keyCtrlD:
				return
			// Space key
			case keySpace:
				player.TogglePause()
			// <- arrow
			case keyLeft, keyDown:
				player.Rewind()
			// -> arrow
			case keyRight, keyUp:
				player.Forward()
			}
		}
	}()
	// player starts playing in its own goroutine
	player.Play()
	// wait for keypresses loop to end
	select {
	case <-player.stopC:
		fmt.Println("\n\nend of session playback")
		return nil
	case err := <-errorCh:
		return trace.Wrap(err)
	}
}

func findActiveDatabases(key *Key) ([]tlsca.RouteToDatabase, error) {
	dbCerts, err := key.DBTLSCertificates()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var databases []tlsca.RouteToDatabase
	for _, cert := range dbCerts {
		tlsID, err := tlsca.FromSubject(cert.Subject, time.Time{})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		// If the cert expiration time is less than 5s consider cert as expired and don't add
		// it to the user profile as an active database.
		if time.Until(cert.NotAfter) < 5*time.Second {
			continue
		}
		if tlsID.RouteToDatabase.ServiceName != "" {
			databases = append(databases, tlsID.RouteToDatabase)
		}
	}
	return databases, nil
}

// getDesktopEventWebURL returns the web UI URL users can access to
// watch a desktop session recording in the browser
func getDesktopEventWebURL(proxyHost string, cluster string, sid *session.ID, events []events.EventFields) string {
	if len(events) < 1 {
		return ""
	}
	start := events[0].GetTimestamp()
	end := events[len(events)-1].GetTimestamp()
	duration := end.Sub(start)

	return fmt.Sprintf("https://%s/web/cluster/%s/session/%s?recordingType=desktop&durationMs=%d", proxyHost, cluster, sid, duration/time.Millisecond)
}

// SearchSessionEvents allows searching for session events with a full pagination support.
func (tc *TeleportClient) SearchSessionEvents(ctx context.Context, fromUTC, toUTC time.Time, pageSize int, order types.EventOrder, max int) ([]apievents.AuditEvent, error) {
	ctx, span := tc.Tracer.Start(
		ctx,
		"teleportClient/SearchSessionEvents",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			attribute.Int("page_size", pageSize),
			attribute.String("from", fromUTC.Format(time.RFC3339)),
			attribute.String("to", toUTC.Format(time.RFC3339)),
		),
	)
	defer span.End()
	proxyClient, err := tc.ConnectToProxy(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxyClient.Close()
	authClient := proxyClient.CurrentCluster()
	defer authClient.Close()

	sessions, err := GetPaginatedSessions(ctx, fromUTC, toUTC,
		pageSize, order, max, authClient)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return sessions, nil
}

func parseMFAMode(in string) (wancli.AuthenticatorAttachment, error) {
	switch in {
	case "auto", "":
		return wancli.AttachmentAuto, nil
	case "platform":
		return wancli.AttachmentPlatform, nil
	case "cross-platform":
		return wancli.AttachmentCrossPlatform, nil
	default:
		return 0, trace.BadParameter("unsupported mfa mode %q", in)
	}
}

// NewKubernetesServiceClient connects to the proxy and returns an authenticated gRPC
// client to the Kubernetes service.
func (tc *TeleportClient) NewKubernetesServiceClient(ctx context.Context, clusterName string) (kubeproto.KubeServiceClient, error) {
	if !tc.TLSRoutingEnabled {
		return nil, trace.BadParameter("kube service is not supported if TLS routing is not enabled")
	}
	// get tlsConfig to dial to proxy.
	tlsConfig, err := tc.LoadTLSConfig()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// Set the ALPN protocols to use when dialing the proxy gRPC mTLS endpoint.
	tlsConfig.NextProtos = []string{string(alpncommon.ProtocolProxyGRPCSecure), http2.NextProtoTLS}

	clt, err := client.New(ctx, client.Config{
		Addrs:            []string{tc.Config.WebProxyAddr},
		DialInBackground: false,
		Credentials: []client.Credentials{
			client.LoadTLS(tlsConfig),
		},
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return kubeproto.NewKubeServiceClient(clt.GetConnection()), nil
}

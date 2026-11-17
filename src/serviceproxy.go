package main

// OS-neutral logic of the Windows agent's proxy (docs/WINDOWS-PROXY.md): what
// the service does with its proxy store at its start, the move of an older
// install's proxy out of the service's registry environment, and the
// argument handling and output of `service configure`, `service proxy` and
// `service capabilities`. The Windows calls are behind small interfaces, so
// all of it is tested on every platform; servicecmd_proxy_windows.go and
// service_windows.go wire it to the registry, the file's ACL and the service
// control manager.

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"reagent/winservice"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// --- at the service's start ---------------------------------------------------

// proxyStartHost is what the service's start needs of Windows: the store
// (read, written atomically with its ACL), the store's ACL, the service's
// registry environment, the process environment and the non-secret
// ProxyConfigured value.
type proxyStartHost interface {
	ReadStore() ([]byte, error)
	WriteStore(data []byte) error
	WriteOld(data []byte) error
	ProtectStore() (repaired bool, err error)
	ServiceEnvironment() ([]string, error)
	Setenv(name, value string) error
	Unsetenv(name string) error
	SetProxyConfigured(configured bool) error
}

// Where the proxy in effect at the start came from.
const (
	proxySourceStore       = "store"
	proxySourceServiceEnv  = "service-environment"
	proxySourceNone        = "none"
	proxySourceUnavailable = "unavailable" // the store could not be read: the process environment stays as Windows gave it
)

// proxyStart is what applyProxyAtStart found and did.
type proxyStart struct {
	Settings winservice.ProxySettings
	Source   string
	// InServiceEnv: the service's registry environment still holds proxy
	// variables (an older agent's install, or the move not finished).
	InServiceEnv bool
}

// applyProxyAtStart reads the store and the service's registry environment,
// decides which proxy is in effect, moves an older install's proxy into the
// store, and sets the agent's process environment, before anything uses the
// network. Every failure is logged once and the start goes on with what it
// has: it never stops the agent.
//
// noProxyFor computes the process's NO_PROXY from the stored one, the
// operator's own entries (effectiveNoProxy).
func applyProxyAtStart(h proxyStartHost, noProxyFor func(user string) string) proxyStart {
	data, err := h.ReadStore()
	storeExists := err == nil
	storeUnreadable := err != nil && !errors.Is(err, os.ErrNotExist)
	if storeUnreadable {
		log.Warn().Err(err).Msg("proxy: could not read the proxy store; going on with the environment Windows gave the service")
	}
	if storeExists {
		repaired, err := h.ProtectStore()
		switch {
		case err != nil:
			log.Warn().Err(err).Msg("proxy: could not restrict the proxy store to SYSTEM and Administrators")
		case repaired:
			log.Info().Msg("proxy: restricted the proxy store to SYSTEM and Administrators")
		}
	}

	env, err := h.ServiceEnvironment()
	if err != nil {
		log.Warn().Err(err).Msg("proxy: could not read the service's registry environment")
	}
	fromEnv, inEnv := winservice.ProxyFromEnvironment(env)

	var result proxyStart
	result.InServiceEnv = inEnv
	switch {
	case inEnv:
		// The values an older agent ran with: they stay in effect, and the
		// store takes them, so that the agent finds them there once they
		// leave the registry.
		result.Settings, result.Source = fromEnv, proxySourceServiceEnv
		switch {
		case storeUnreadable:
		case storeExists && winservice.ParseProxyEnv(data) == fromEnv:
		case storeExists:
			if err := h.WriteOld(data); err != nil {
				log.Warn().Err(err).Msg("proxy: could not keep the proxy store's earlier content in " + winservice.ProxyEnvOldName)
			}
			if err := h.WriteStore([]byte(winservice.RenderProxyEnv(fromEnv))); err != nil {
				log.Warn().Err(err).Msg("proxy: could not write the proxy store from the service's registry environment")
			} else {
				log.Info().Msg("proxy: the proxy store differed from the service's registry environment, which was in effect; the store holds those values now (its earlier content is in " + winservice.ProxyEnvOldName + ")")
			}
		case fromEnv.Configured():
			if err := h.WriteStore([]byte(winservice.RenderProxyEnv(fromEnv))); err != nil {
				log.Warn().Err(err).Msg("proxy: could not create the proxy store from the service's registry environment")
			} else {
				log.Info().Msg("proxy: created the proxy store from the service's registry environment")
			}
		}
	case storeExists:
		result.Settings, result.Source = winservice.ParseProxyEnv(data), proxySourceStore
	case storeUnreadable:
		result.Source = proxySourceUnavailable
		return result
	default:
		result.Source = proxySourceNone
	}

	if result.Settings.Configured() {
		inEffect := result.Settings
		inEffect.NoProxy = noProxyFor(result.Settings.NoProxy)
		for _, v := range inEffect.Values() {
			if v[1] == "" {
				err = h.Unsetenv(v[0])
			} else {
				err = h.Setenv(v[0], v[1])
			}
			if err != nil {
				log.Warn().Err(err).Msgf("proxy: could not set %s in the agent's environment", v[0])
			}
		}
		log.Info().Str("http", redactedProxyURL(result.Settings.HTTP)).Str("https", redactedProxyURL(result.Settings.HTTPS)).
			Str("no_proxy", inEffect.NoProxy).Str("from", result.Source).Msg("proxy: the agent uses a proxy")
	} else {
		log.Info().Str("from", result.Source).Msg("proxy: no proxy configured for the agent")
	}
	if err := h.SetProxyConfigured(result.Settings.Configured()); err != nil {
		log.Warn().Err(err).Msg("proxy: could not record whether a proxy is configured")
	}
	return result
}

// proxyCleanupHost is what the move of the proxy out of the service's
// registry environment needs.
type proxyCleanupHost interface {
	ReadStore() ([]byte, error)
	ServiceEnvironment() ([]string, error)
	SetServiceEnvironment(env []string) error
}

// removeProxyFromServiceEnvironment takes the proxy variables out of the
// service's registry environment, keeping every other entry. It does so only
// when the store holds the same values, so that the next start finds them
// there; otherwise it leaves the registry alone. A failed write is tried
// attempts times, pause apart, then left to the next start.
func removeProxyFromServiceEnvironment(h proxyCleanupHost, attempts int, pause time.Duration, sleep func(time.Duration)) bool {
	for attempt := 1; ; attempt++ {
		env, err := h.ServiceEnvironment()
		if err == nil {
			fromEnv, inEnv := winservice.ProxyFromEnvironment(env)
			if !inEnv {
				return true
			}
			var data []byte
			data, err = h.ReadStore()
			switch {
			case errors.Is(err, os.ErrNotExist) && !fromEnv.Configured():
				err = nil // empty variables only: nothing to keep
			case err != nil:
			case winservice.ParseProxyEnv(data) != fromEnv:
				log.Warn().Msg("proxy: the proxy store does not hold the values of the service's registry environment; they stay there until a start finds both alike")
				return false
			}
			if err == nil {
				err = h.SetServiceEnvironment(winservice.WithoutProxyVariables(env))
				if err == nil {
					log.Info().Msg("proxy: removed the proxy variables from the service's registry environment; the agent reads them from its proxy store")
					return true
				}
			}
		}
		if attempt >= attempts {
			log.Warn().Err(err).Msgf("proxy: could not remove the proxy variables from the service's registry environment after %d attempts; the next start tries again", attempts)
			return false
		}
		sleep(pause)
	}
}

// --- shared by the commands -------------------------------------------------------

// usageError is a wrong command line (exit 2), preconditionError a state
// that does not allow the command (exit 3). Neither changed anything.
type usageError struct{ error }
type preconditionError struct{ error }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

func preconditionf(format string, args ...any) error {
	return preconditionError{fmt.Errorf(format, args...)}
}

// Exit codes of `service configure` and `service proxy` (docs/WINDOWS-PROXY.md).
const (
	exitOK            = 0
	exitFailed        = 1
	exitUsage         = 2
	exitPrecondition  = 3
	exitWrittenNotRun = 5
)

// exitCodeOf maps an error of a command to its exit code.
func exitCodeOf(err error) int {
	var usage usageError
	var precondition preconditionError
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.As(err, &usage):
		return exitUsage
	case errors.As(err, &precondition):
		return exitPrecondition
	}
	return exitFailed
}

// maxProxyPasswordBytes is as much of -proxy-password-file as is read.
const maxProxyPasswordBytes = 4096

// checkProxyURL checks a proxy URL: http, https, socks5 or socks5h, a host,
// nothing after it but "/", no quote, space, backslash or control character.
// One without a scheme is http. No error repeats the value: it may hold a
// password.
func checkProxyURL(value string) (*url.URL, error) {
	if value == "" {
		return nil, errors.New("needs a URL such as http://proxy.example.com:3128")
	}
	bad := errors.New("is not a proxy URL such as http://proxy.example.com:3128 (http, https or socks5)")
	if strings.ContainsFunc(value, func(r rune) bool { return r <= ' ' || r == 0x7f || strings.ContainsRune("'\"\\`", r) }) {
		return nil, bad
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.Opaque != "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return nil, bad
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return u, nil
	}
	return nil, bad
}

// checkNoProxy checks a NO_PROXY list: entries without spaces, quotes or
// control characters.
func checkNoProxy(value string) error {
	for _, entry := range strings.Split(value, ",") {
		if strings.ContainsFunc(strings.TrimSpace(entry), func(r rune) bool { return r <= ' ' || r == 0x7f || strings.ContainsRune("'\"\\`", r) }) {
			return errors.New("must be host names, addresses or domains separated by commas")
		}
	}
	return nil
}

// takesPassword: the URL names a user without a password part
// (http://user@proxy:3128); one of user:@ gives the empty password itself.
func takesPassword(u *url.URL) bool {
	if u.User == nil {
		return false
	}
	_, set := u.User.Password()
	return !set
}

// checkProxyURLFlag checks the value of a URL flag that must not carry a
// password; needsPassword reports a user without a password part.
func checkProxyURLFlag(name, value string) (needsPassword bool, err error) {
	u, err := checkProxyURL(value)
	if err != nil {
		return false, usagef("-%s %v", name, err)
	}
	if password, set := u.User.Password(); set && password != "" {
		return false, usagef("-%s holds a password, and a command line is visible to every user of this machine: give the URL without it (http://user@proxy:3128) and the password with -proxy-password-file", name)
	}
	return takesPassword(u), nil
}

// withPassword is a checked proxy URL with password when it names a user
// without a password part; one of user:@ keeps its empty password.
func withPassword(value, password string) string {
	u, err := checkProxyURL(value)
	if err != nil {
		return value
	}
	if takesPassword(u) {
		u.User = url.UserPassword(u.User.Username(), password)
	}
	return u.String()
}

// passwordInto puts password into a stored URL that names a user (with or
// without a password); ok is false when it names none.
func passwordInto(raw, password string) (string, bool) {
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.User == nil {
		return raw, false
	}
	u.User = url.UserPassword(u.User.Username(), password)
	return u.String(), true
}

// shownProxyURL is a proxy URL without its password, and whether it had one.
// The empty password (user:@) is none and shows as it is.
func shownProxyURL(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "<a URL that does not parse>", false
	}
	if password, set := u.User.Password(); set && password != "" {
		u.User = url.User(u.User.Username())
		return u.String(), true
	}
	return u.String(), false
}

// redactedProxyURL is shownProxyURL for a log line.
func redactedProxyURL(raw string) string {
	shown, password := shownProxyURL(raw)
	if password {
		return shown + " (with a password)"
	}
	return shown
}

// readProxyPassword reads the password: the first line of r. No error says
// what it holds.
func readProxyPassword(r io.Reader, name string) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxProxyPasswordBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", name, err)
	}
	if len(data) > maxProxyPasswordBytes {
		return "", usagef("%s is larger than -proxy-password-file takes (%d bytes)", name, maxProxyPasswordBytes)
	}
	line, _, err := bufio.NewReader(strings.NewReader(string(data))).ReadLine()
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	password := strings.TrimSuffix(string(line), "\r")
	switch {
	case password == "":
		return "", usagef("%s is empty: put the proxy password on its first line; an empty password is given in the URL, as http://user:@proxy:3128, without -proxy-password-file", name)
	case strings.ContainsFunc(password, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "", usagef("%s holds a control character in its first line: put the proxy password alone on it", name)
	}
	return password, nil
}

// openProxyPassword reads the password from the file named by
// -proxy-password-file, or from stdin for "-".
func openProxyPassword(path string, stdin io.Reader) (string, error) {
	if path == "-" {
		return readProxyPassword(stdin, "standard input")
	}
	f, err := os.Open(path) //nolint:gosec // G304: the file named by -proxy-password-file
	if err != nil {
		return "", usagef("-proxy-password-file: %v", err)
	}
	defer func() { _ = f.Close() }()
	return readProxyPassword(f, path)
}

// proxyReportLines are the proxy.* output lines of `service configure` and
// `service proxy`, without a password.
//
// effective is the NO_PROXY list the agent's process uses: the operator's
// entries (proxy.no_proxy) and what the device reaches directly.
func proxyReportLines(s winservice.ProxySettings, store string, effective string) []string {
	http, httpPassword := shownProxyURL(s.HTTP)
	https, httpsPassword := shownProxyURL(s.HTTPS)
	return []string{
		fmt.Sprintf("proxy.configured=%t", s.Configured()),
		"proxy.http=" + http,
		"proxy.https=" + https,
		"proxy.no_proxy=" + s.NoProxy,
		"proxy.no_proxy_effective=" + effective,
		fmt.Sprintf("proxy.password=%t", httpPassword || httpsPassword),
		"proxy.store=" + store,
	}
}

// --- service capabilities ---------------------------------------------------------

// serviceCapabilities are the lines of `service capabilities`: what a caller
// may use without comparing versions.
var serviceCapabilities = []string{"service-capabilities", "proxy-store", "service-configure", "service-proxy"}

// --- service configure ------------------------------------------------------------

type serviceConfigureOptions struct {
	HTTP, HTTPS, NoProxy string
	PasswordFile         string
	Remove               bool
	NoRestart            bool
	given                map[string]bool
}

func (o *serviceConfigureOptions) Given(name string) bool { return o.given[name] }

const serviceConfigureUsage = "usage: device-agent.exe service configure [-http-proxy URL] [-https-proxy URL] [-no-proxy HOSTS] [-proxy-password-file PATH|-] [-remove-proxy] [-no-restart]"

// parseServiceConfigureFlags parses and checks `service configure`'s flags.
// Every error is a usageError (or flag.ErrHelp for -h).
func parseServiceConfigureFlags(args []string, output io.Writer) (*serviceConfigureOptions, error) {
	flags := flag.NewFlagSet("service configure", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(output, serviceConfigureUsage)
		flags.PrintDefaults()
	}
	var o serviceConfigureOptions
	flags.StringVar(&o.HTTP, "http-proxy", "", "the HTTP proxy, such as http://proxy:3128, or http://user@proxy:3128 without the password")
	flags.StringVar(&o.HTTPS, "https-proxy", "", "the HTTPS proxy, such as http://proxy:3128, or http://user@proxy:3128 without the password")
	flags.StringVar(&o.NoProxy, "no-proxy", "", "comma-separated hosts reached directly, kept as given; the agent always adds loopback, this machine's names and networks and the device's local hosts")
	flags.StringVar(&o.PasswordFile, "proxy-password-file", "", "a file whose first line is the password of the user in the proxy URLs, or - for standard input")
	flags.BoolVar(&o.Remove, "remove-proxy", false, "remove the proxy settings: the agent connects directly")
	flags.BoolVar(&o.NoRestart, "no-restart", false, "write the settings without restarting the service; they apply at its next start")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, usageError{err}
	}
	if flags.NArg() > 0 {
		return nil, usagef("unexpected argument %q\n%s", flags.Arg(0), serviceConfigureUsage)
	}
	o.given = map[string]bool{}
	flags.Visit(func(f *flag.Flag) { o.given[f.Name] = true })

	if o.Remove {
		for _, name := range []string{"http-proxy", "https-proxy", "no-proxy", "proxy-password-file"} {
			if o.given[name] {
				return nil, usagef("-remove-proxy removes the proxy settings and takes no other proxy flag, not -%s", name)
			}
		}
	}
	if !o.Remove && !o.given["http-proxy"] && !o.given["https-proxy"] && !o.given["no-proxy"] && !o.given["proxy-password-file"] {
		return nil, usagef("nothing to change: give -http-proxy, -https-proxy, -no-proxy, -proxy-password-file or -remove-proxy\n%s", serviceConfigureUsage)
	}
	users := 0
	for _, s := range []struct{ name, value string }{{"http-proxy", o.HTTP}, {"https-proxy", o.HTTPS}} {
		if !o.given[s.name] {
			continue
		}
		needs, err := checkProxyURLFlag(s.name, s.value)
		if err != nil {
			return nil, err
		}
		if needs {
			users++
			if !o.given["proxy-password-file"] {
				return nil, usagef("-%s names a user: give the password with -proxy-password-file (a file, or - for standard input), or an empty password in the URL (http://user:@proxy:3128)", s.name)
			}
		}
	}
	if o.given["proxy-password-file"] {
		urls := o.given["http-proxy"] || o.given["https-proxy"]
		switch {
		case o.PasswordFile == "":
			return nil, usagef("-proxy-password-file needs the name of a file, or - for standard input")
		case urls && users == 0:
			return nil, usagef("-proxy-password-file needs a proxy URL with a user and no password part: -http-proxy or -https-proxy http://user@proxy:3128 (http://user:@proxy:3128 gives the empty password and takes no file)")
		}
	}
	if o.given["no-proxy"] {
		if err := checkNoProxy(o.NoProxy); err != nil {
			return nil, usagef("-no-proxy %v", err)
		}
	}
	return &o, nil
}

// configuredProxy is what `service configure` makes of the stored settings
// (the same rules as `ironflock agent configure` on Linux): a flag given
// changes its setting, the others stay; the password goes into the URLs
// given that name a user, or with no URL given into the stored ones that do.
// NO_PROXY holds the operator's own entries: those of -no-proxy when it is
// given, else the stored ones (none for a device without a proxy); the agent
// adds what the device reaches directly at its start. password is read by
// the caller when -proxy-password-file is given.
func configuredProxy(o *serviceConfigureOptions, from winservice.ProxySettings, password string) (winservice.ProxySettings, error) {
	if o.Remove {
		return winservice.ProxySettings{}, nil
	}
	urls := o.Given("http-proxy") || o.Given("https-proxy")
	if !urls && !from.Configured() {
		return winservice.ProxySettings{}, preconditionf("no proxy is set for the agent: give it with -http-proxy or -https-proxy")
	}
	to := from
	if !urls && o.Given("proxy-password-file") {
		var httpUser, httpsUser bool
		to.HTTP, httpUser = passwordInto(from.HTTP, password)
		to.HTTPS, httpsUser = passwordInto(from.HTTPS, password)
		if !httpUser && !httpsUser {
			return winservice.ProxySettings{}, preconditionf("no stored proxy URL names a user for the password of -proxy-password-file: give the URL with its user, such as -http-proxy http://user@proxy:3128")
		}
	}
	if o.Given("http-proxy") {
		to.HTTP = withPassword(o.HTTP, password)
	}
	if o.Given("https-proxy") {
		to.HTTPS = withPassword(o.HTTPS, password)
	}
	switch {
	case o.Given("no-proxy"):
		to.NoProxy = userNoProxy(o.NoProxy)
	case !from.Configured():
		to.NoProxy = ""
	}
	if !to.Configured() {
		return winservice.ProxySettings{}, nil
	}
	return to, nil
}

// underProcess reports whether target is self or one of its ancestors in
// parent (process id to parent process id). A parent id the operating system
// reused can make a chain look longer; the walk stops at a loop or after 64
// steps.
func underProcess(parent map[uint32]uint32, self, target uint32) bool {
	if target == 0 {
		return false
	}
	seen := map[uint32]bool{}
	for pid := self; pid != 0 && !seen[pid] && len(seen) < 64; pid = parent[pid] {
		if pid == target {
			return true
		}
		seen[pid] = true
	}
	return false
}

// userNoProxy is the operator's -no-proxy list as the store keeps it:
// entries trimmed, empty ones dropped, otherwise as given.
func userNoProxy(value string) string {
	var entries []string
	for _, entry := range strings.Split(value, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			entries = append(entries, entry)
		}
	}
	return strings.Join(entries, ",")
}

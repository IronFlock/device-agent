package apps

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"reagent/common"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// What a release build pushed: pushed_digests of the PUBLISHED report
// (store.UpdateRemoteAppState), {"<image reference as pushed>":
// "sha256:<manifest digest>"}. The backend compares it with what the tag holds
// when it publishes the build, so that another device's later push of the
// same tag is not published under this build. A digest the agent cannot tell
// for certain is left out, never guessed.

// pushDigestCapture reads, as a push's progress stream passes through it, the
// digest the registry answered for the pushed manifest, once per pushed tag:
// the Engine API's aux message {"aux":{"Tag":"<tag>","Digest":"sha256:…",…}},
// and the status line "<tag>: digest: sha256:… size: <n>". The classic image
// store sends both; the containerd image store (Docker 29) sends only the
// status line. Either is the digest of the manifest this push wrote under the
// tag, as the registry accepted it.
type pushDigestCapture struct {
	mu      sync.Mutex
	pending []byte
	digests map[string]map[string]bool // tag -> digests seen
}

func (c *pushDigestCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, p...)
	for {
		newline := bytes.IndexByte(c.pending, '\n')
		if newline == -1 {
			break
		}
		c.readLine(c.pending[:newline])
		c.pending = c.pending[newline+1:]
	}
	return len(p), nil
}

func (c *pushDigestCapture) readLine(line []byte) {
	var message struct {
		Status string `json:"status"`
		Aux    *struct {
			Tag    string `json:"Tag"`
			Digest string `json:"Digest"`
		} `json:"aux"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &message) != nil {
		return
	}
	if message.Aux != nil {
		c.add(message.Aux.Tag, message.Aux.Digest)
	}
	if match := pushStatusDigestPattern.FindStringSubmatch(strings.TrimSpace(message.Status)); match != nil {
		c.add(match[1], match[2])
	}
}

// pushStatusDigestPattern is the status line a push ends a tag with.
var pushStatusDigestPattern = regexp.MustCompile(`^(\S+): digest: (\S+) size: [0-9]+$`)

func (c *pushDigestCapture) add(tag, digest string) {
	if c.digests == nil {
		c.digests = map[string]map[string]bool{}
	}
	if c.digests[tag] == nil {
		c.digests[tag] = map[string]bool{}
	}
	c.digests[tag][digest] = true
}

// digest is the digest the push answered for tag: exactly one, a sha256
// digest; ok is false otherwise.
func (c *pushDigestCapture) digest(tag string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) > 0 {
		// a last line without a newline
		c.readLine(c.pending)
		c.pending = nil
	}
	return onlyDigest(c.digests[tag])
}

// onlyDigest is the one sha256 digest of a set, ok false for none or several.
func onlyDigest(digests map[string]bool) (string, bool) {
	if len(digests) != 1 {
		return "", false
	}
	for digest := range digests {
		if imageDigestPattern.MatchString(digest) {
			return digest, true
		}
	}
	return "", false
}

// capturingReadCloser passes a push stream on and lets capture read it.
type capturingReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r capturingReadCloser) Close() error { return r.closer.Close() }

// capturePushDigest wraps a push's progress stream so that the pushed
// digest can be read from capture once the stream was read to its end.
func capturePushDigest(reader io.ReadCloser) (io.ReadCloser, *pushDigestCapture) {
	capture := &pushDigestCapture{}
	if reader == nil {
		return nil, capture
	}
	return capturingReadCloser{Reader: io.TeeReader(reader, capture), closer: reader}, capture
}

// pushedDigestOf is the digest the push of ref (repository:tag) answered,
// read from its stream (capture). ok is false where it cannot be told.
func pushedDigestOf(ref string, capture *pushDigestCapture) (string, bool) {
	if capture == nil {
		return "", false
	}
	_, tag := splitImageTag(ref)
	if tag == "" {
		return "", false
	}
	return capture.digest(tag)
}

// repoDigestOf is the digest under which the local image ref names is in its
// repository, after `docker compose push` pushed it (the CLI's output gives no
// dependable digest): the image's RepoDigests entry for exactly that
// repository. A push records the digest the registry answered there (both
// image stores). Only one entry counts: where the image also carries the
// digest of an earlier push or pull of that repository, which of them this
// push wrote cannot be told, and ok is false.
func (sm *StateMachine) repoDigestOf(ref string) (string, bool) {
	repository, tag := splitImageTag(ref)
	if tag == "" || strings.Contains(ref, "@") {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	image, err := sm.Container.GetImage(ctx, repository, tag)
	if err != nil {
		log.Debug().Err(err).Str("image", ref).Msg("could not read the pushed image's digest")
		return "", false
	}
	digests := map[string]bool{}
	for _, repoDigest := range image.RepoDigests {
		if digest, found := strings.CutPrefix(repoDigest, repository+"@"); found {
			digests[digest] = true
		}
	}
	return onlyDigest(digests)
}

// composePushedImages are the images `docker compose push` pushes from the
// compose file at path: every service that is built and names an image, as
// the file names it.
func composePushedImages(path string) []string {
	content, err := os.ReadFile(path) //nolint:gosec // G304: the agent's own compose file, written by SetupComposeFiles
	if err != nil {
		return nil
	}
	var compose map[string]interface{}
	if json.Unmarshal(content, &compose) != nil {
		return nil
	}
	services, _ := compose["services"].(map[string]interface{})
	refs := []string{}
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(map[string]interface{})
		if !ok || service["build"] == nil {
			continue
		}
		if ref, _ := service["image"].(string); ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}

// setPushedDigests keeps what a release build pushed for its PUBLISHED
// report; nil or empty: nothing is known.
func setPushedDigests(app *common.App, digests map[string]string) {
	app.StateLock.Lock()
	defer app.StateLock.Unlock()
	if len(digests) == 0 {
		app.PushedDigests = nil
		return
	}
	app.PushedDigests = digests
}

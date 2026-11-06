package osupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"reagent/updatesig"
	"regexp"

	"github.com/Masterminds/semver"
)

// CatalogPath is the catalog below the update base (the .flock's update_url
// or the cloud's instance-registry.ironflock.com/dl).
const CatalogPath = "flockos/manifest.json"

// CatalogSubject is what the catalog is signed as (FlockOS ADR-0009): its own
// context, so no other signed document of the update trees can stand in for
// it, and no catalog for one of them.
var CatalogSubject = updatesig.Subject{Context: "flockos-manifest:v1", Line: CatalogPath}

// FLOCKOS_CATALOG_PUBLIC_KEYS lists the keys the catalog may be signed with,
// in the format of updatesig.AGENT_UPDATE_PUBLIC_KEYS: one standard base64
// raw Ed25519 key per line, "#" starts a comment. A key of its own, not an
// agent update key: only FlockOS release runs may sign with it.
//
// The line below is the Cloud KMS key ironflock-release/flockos-catalog,
// version 1 (FlockOS WP-2.6; pki/flockos-catalog.pub and pki/README.md in
// IronFlock/flockos give its PEM form and how to check it against KMS).
// Rotate it as the agent update keys: a release that lists both keys reaches
// the fleet before FlockOS signs with the new one.
const FLOCKOS_CATALOG_PUBLIC_KEYS = `
wFLa1T7iHYi20X8JVu/O6JUzBGmW2+tjo7QVMuxsmQg= # key f5e1308e, Cloud KMS ironflock-release/flockos-catalog v1
`

// catalogKeys returns the trusted keys; tests replace it.
var catalogKeys = func() ([]updatesig.PublicKey, error) {
	keys, err := updatesig.ParsePublicKeys(FLOCKOS_CATALOG_PUBLIC_KEYS)
	if errors.Is(err, updatesig.ErrNoTrustedKeys) {
		return nil, nil
	}
	return keys, err
}

// ErrNoCatalogKey: this agent trusts no catalog key yet.
var ErrNoCatalogKey = errors.New("this agent trusts no FlockOS catalog key yet (FLOCKOS_CATALOG_PUBLIC_KEYS)")

// Artifact is one published file.
type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Bundle is a RAUC bundle.
type Bundle struct {
	Artifact
	Compatible string `json:"compatible"`
}

// Image is one image of a release.
type Image struct {
	Target       string   `json:"target"`
	Variant      string   `json:"variant"`
	Kernel       string   `json:"kernel"`
	Architecture string   `json:"architecture"`
	Image        Artifact `json:"image"`
	Bundle       *Bundle  `json:"bundle"`
}

// Release is one FlockOS version.
type Release struct {
	BuildID   string           `json:"build_id"`
	BuildTime string           `json:"buildtime"`
	MinAgent  string           `json:"min_agent"`
	Images    map[string]Image `json:"images"`
}

// Catalog is the payload of dl/flockos/manifest.json, schema 1. FlockFlasher's
// boards view is not read here.
type Catalog struct {
	Schema   int                `json:"schema"`
	IssuedAt string             `json:"issued_at"`
	Channels map[string]string  `json:"channels"`
	Releases map[string]Release `json:"releases"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// versionRe is a release version as the catalog schema has it: safe as a
// path segment.
var versionRe = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)

// OpenCatalog verifies a published catalog (the updatesig envelope) and
// returns it with the id of the key that signed it.
func OpenCatalog(data []byte) (Catalog, string, error) {
	keys, err := catalogKeys()
	if err != nil {
		return Catalog{}, "", err
	}
	if len(keys) == 0 {
		return Catalog{}, "", ErrNoCatalogKey
	}
	payload, keyID, err := updatesig.OpenEnvelope(keys, CatalogSubject, data)
	if err != nil {
		return Catalog{}, "", err
	}
	var c Catalog
	if err := json.Unmarshal(payload, &c); err != nil {
		return Catalog{}, "", fmt.Errorf("catalog payload: %w", err)
	}
	if c.Schema != 1 {
		return Catalog{}, "", fmt.Errorf("catalog schema %d: this agent reads schema 1; an agent update reads it", c.Schema)
	}
	if c.Channels["production"] == "" {
		return Catalog{}, "", errors.New("catalog names no production release")
	}
	for channel, version := range c.Channels {
		if _, ok := c.Releases[version]; !ok {
			return Catalog{}, "", fmt.Errorf("catalog channel %s names %s, which it does not list", channel, version)
		}
	}
	return c, keyID, nil
}

// Offer is the release the device's channel names for its image.
type Offer struct {
	Channel string
	Version string
	Release Release
	Bundle  Bundle
	// Newer: the offered version is newer than the running one.
	Newer bool
}

// ErrNotOffered: the release has no image for this device's IMAGE_ID.
var ErrNotOffered = errors.New("the release has no image for this device")

// Offer returns the release channel names for the image running.ImageID; a
// channel the catalog does not carry follows production.
func (c Catalog) Offer(running Running, channel string) (Offer, error) {
	version := c.Channels[channel]
	if version == "" {
		channel, version = "production", c.Channels["production"]
	}
	release := c.Releases[version]
	image, ok := release.Images[running.ImageID]
	if !ok {
		return Offer{}, fmt.Errorf("%w: %s %s has no %s", ErrNotOffered, channel, version, running.ImageID)
	}
	if image.Bundle == nil {
		return Offer{}, fmt.Errorf("%w: %s %s has no bundle for %s", ErrNotOffered, channel, version, running.ImageID)
	}
	b := *image.Bundle
	if b.Compatible != running.ImageID {
		return Offer{}, fmt.Errorf("the %s bundle of %s is compatible %q, not %q", channel, version, b.Compatible, running.ImageID)
	}
	if !sha256Hex.MatchString(b.SHA256) || b.Size <= 0 || b.URL == "" {
		return Offer{}, fmt.Errorf("the %s bundle of %s has no usable url, sha256 and size", channel, version)
	}
	newer, err := isNewer(version, running.ImageVersion)
	if err != nil {
		return Offer{}, err
	}
	return Offer{Channel: channel, Version: version, Release: release, Bundle: b, Newer: newer}, nil
}

// isNewer reports whether version is newer than current (semantic versions).
func isNewer(version, current string) (bool, error) {
	v, err := semver.NewVersion(version)
	if err != nil {
		return false, fmt.Errorf("offered version %q: %w", version, err)
	}
	c, err := semver.NewVersion(current)
	if err != nil {
		return false, fmt.Errorf("running version %q: %w", current, err)
	}
	return v.GreaterThan(c), nil
}

// ErrAgentTooOld: the release needs a newer device-agent first.
var ErrAgentTooOld = errors.New("the release needs a newer device-agent")

// CheckAgent refuses a release whose min_agent is newer than agentVersion.
// A development agent (an unparsable version) is not held back.
func (o Offer) CheckAgent(agentVersion string) error {
	if o.Release.MinAgent == "" {
		return nil
	}
	agent, err := semver.NewVersion(agentVersion)
	if err != nil {
		return nil
	}
	min, err := semver.NewVersion(o.Release.MinAgent)
	if err != nil {
		return fmt.Errorf("min_agent %q: %w", o.Release.MinAgent, err)
	}
	if agent.LessThan(min) {
		return fmt.Errorf("%w: FlockOS %s needs agent %s, this is %s", ErrAgentTooOld, o.Version, o.Release.MinAgent, agentVersion)
	}
	return nil
}

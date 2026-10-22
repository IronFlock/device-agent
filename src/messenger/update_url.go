package messenger

import "reagent/config"

// applianceHostBoardModel marks the appliance HOST's own agent in its .flock
// (Board.Model); see agent.go, which keys the registry-GC lever on the same.
const applianceHostBoardModel = "appliance"

// applyHandedOffUpdateURL applies the OTA base URL an appliance backend hands
// off on every heartbeat (`agentUpdateURL`) to cfg.UpdateURL, so devices
// provisioned before the appliance's local agent mirror existed switch over
// without a manual .flock swap. It reports whether cfg changed — the caller
// persists the .flock only then — and the URL it saw.
//
// Only a non-empty string that differs from the current value changes cfg.
// The appliance host's own agent (Board.Model == "appliance") is exempt: the
// heartbeat value is the appliance's LAN address, unreachable on the host
// itself once REGISTRY_BIND is loopback in domain mode, so it keeps the
// installer-written http://localhost:<port>/dl. The cloud backend sends
// instance-registry.ironflock.com/dl, so cloud agents move off any older base
// in their .flock the same way. updateBaseURL() precedence (.flock update_url
// over --remoteUpdateURL) is unchanged.
func applyHandedOffUpdateURL(cfg *config.ReswarmConfig, v any) (changed bool, url string) {
	url, ok := v.(string)
	if !ok || url == "" || cfg == nil {
		return false, ""
	}
	if cfg.Board.Model == applianceHostBoardModel || cfg.UpdateURL == url {
		return false, url
	}
	cfg.UpdateURL = url
	return true, url
}

package apps

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"reagent/common"
	"reagent/config"
	"strings"
)

// Per-app WAMP credentials (cross-app data access).
//
// Every app container on a device used to authenticate as the DEVICE — authid
// = serial number, and the WAMP-CRA "secret" was that same serial — so the
// platform could only ever authorize the device, never the app. A co-located
// app therefore reached its neighbour's data realm with the full swarm_device
// role before the user's data-access grant was consulted at all. These
// credentials give each container an identity the authenticator can attribute:
//
//	authid  app-{app_key}-{stage}-e{epoch}@{serial}
//	secret  base64(HMAC-SHA256(app_cred_key, "ironflock/app-cred/v1|{serial}|{app_key}|{STAGE}|{epoch}"))
//
// app_cred_key is a per-DEVICE key the agent fetches once from
// reswarm.devices.get_app_cred_key and keeps in memory — it is never written
// into a container. Deliberately not derived from ReswarmConfig.Secret: that
// value used to be injected into every container (see the DEVICE_SECRET
// removal in buildDefaultEnvironmentVariables), so deriving from it would let
// any co-located app mint its siblings' credentials.
//
// The derivation MUST stay byte-identical to REaccounting's
// device.f_authenticate_userapp, which recomputes the same secret to verify
// the CRA signature. Both sides HMAC over the UTF-8 bytes of the stored
// base64 key string itself (no base64-decode round trip).
const appCredMessagePrefix = "ironflock/app-cred/v1" //nolint:gosec // G101: a message prefix, not a credential

// appCredentialAuthID builds the wire authid. The epoch travels inside it
// because the router hands the authenticator exactly one secret back, so a
// rotation grace window is only possible if the client says which generation
// it holds.
func appCredentialAuthID(serialNumber string, appKey uint64, stage common.Stage, epoch uint64) string {
	return fmt.Sprintf("app-%d-%s-e%d@%s", appKey, strings.ToLower(string(stage)), epoch, serialNumber)
}

func appCredentialSecret(appCredKey string, serialNumber string, appKey uint64, stage common.Stage, epoch uint64) string {
	msg := fmt.Sprintf("%s|%s|%d|%s|%d",
		appCredMessagePrefix, serialNumber, appKey, strings.ToUpper(string(stage)), epoch)
	mac := hmac.New(sha256.New, []byte(appCredKey))
	mac.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// appCredential returns the (authid, secret) pair for one app container.
// Returns empty strings when the per-device key is not available yet (first
// boot before the backend answered, or an offline device): callers MUST then
// leave any existing credential files untouched rather than writing blanks,
// so a backend outage cannot brick running apps.
func appCredential(cfg *config.Config, appCredKey string, payload common.TransitionPayload) (authID string, secret string) {
	if appCredKey == "" || cfg.ReswarmConfig.SerialNumber == "" || payload.AppKey == 0 {
		return "", ""
	}

	// absent credential row on the backend means epoch 1 by definition
	epoch := payload.AppCredEpoch
	if epoch == 0 {
		epoch = 1
	}

	serial := cfg.ReswarmConfig.SerialNumber
	return appCredentialAuthID(serial, payload.AppKey, payload.Stage, epoch),
		appCredentialSecret(appCredKey, serial, payload.AppKey, payload.Stage, epoch)
}

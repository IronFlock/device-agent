package firmware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestDecodeRelease(t *testing.T) {
	r := decodeRelease(variants(map[string]interface{}{
		"Version":     "1.2.3",
		"Summary":     "The firmware",
		"Description": "<p>Fixes &amp; more.</p><ul><li>One</li>\n<li>Two</li></ul>",
		"Vendor":      "ACME",
		"Urgency":     uint32(3),
		"Size":        uint64(1024),
		"Created":     uint64(1700000000),
		"Issues":      []string{"CVE-2026-1"},
		"Locations":   []string{"https://cdn.example/a.cab"},
		"Checksum":    "aaaa,bbbb",
	}))
	assert.Equal(t, "Fixes & more. One Two", r.Description)
	assert.Equal(t, "high", r.Urgency)
	assert.Equal(t, []string{"aaaa", "bbbb"}, r.Checksums)
	assert.Equal(t, []string{"https://cdn.example/a.cab"}, r.Locations)

	t.Run("an older fwupd's single address", func(t *testing.T) {
		r := decodeRelease(variants(map[string]interface{}{"Uri": "https://cdn.example/b.cab"}))
		assert.Equal(t, []string{"https://cdn.example/b.cab"}, r.Locations)
	})
}

func TestBestChecksum(t *testing.T) {
	sha1Sum := "0123456789012345678901234567890123456789"
	sha256Sum := sum([]byte("x"))
	got, _, err := bestChecksum([]string{sha1Sum, sha256Sum})
	require.NoError(t, err)
	assert.Equal(t, sha256Sum, got, "the strongest one")

	_, _, err = bestChecksum([]string{"short"})
	require.Error(t, err)
	_, _, err = bestChecksum(nil)
	require.Error(t, err)
}

func TestFetchRefusesPlainHTTP(t *testing.T) {
	err := fetch(context.Background(), "http://cdn.example/a.cab", 10, io.Discard, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing")
}

// --- over a fake fwupd on a private bus ---------------------------------------

// busWithFwupd puts fake on a private bus, and serves files over https.
func busWithFwupd(t *testing.T, fake *fakeFwupd, files map[string][]byte) (*dbus.Conn, string) {
	t.Helper()
	addr := privateBus(t)
	serve(t, addr, fake)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	oldClient, oldDir := httpClient, TempDir
	httpClient, TempDir = srv.Client(), t.TempDir()
	t.Cleanup(func() { httpClient, TempDir = oldClient, oldDir })
	return client(t, addr), srv.URL
}

func updatable(id string, extra map[string]interface{}) map[string]dbus.Variant {
	d := map[string]interface{}{"DeviceId": id, "Name": "Device " + id, "Version": "1", "Flags": uint64(1 << 1)}
	for k, v := range extra {
		d[k] = v
	}
	return variants(d)
}

func release(version string, extra map[string]interface{}) map[string]dbus.Variant {
	d := map[string]interface{}{"Version": version}
	for k, v := range extra {
		d[k] = v
	}
	return variants(d)
}

func TestSnapshot(t *testing.T) {
	fake := &fakeFwupd{
		devices: []map[string]dbus.Variant{
			updatable("a", nil),
			updatable("b", nil),
			variants(map[string]interface{}{"DeviceId": "c", "Name": "Hub"}),
		},
		upgrades: map[string][]map[string]dbus.Variant{
			"a": {release("3", nil), release("2", nil)},
		},
		history: []map[string]dbus.Variant{
			variants(map[string]interface{}{"DeviceId": "a", "Name": "Old", "Version": "0", "Modified": uint64(100), "UpdateState": uint32(2)}),
			variants(map[string]interface{}{
				"DeviceId": "a", "Name": "New", "Version": "1", "Modified": uint64(200), "UpdateState": uint32(3), "UpdateError": "no space",
				"Release": []map[string]dbus.Variant{release("2", nil)},
			}),
		},
	}
	conn, _ := busWithFwupd(t, fake, nil)

	inv, err := snapshot(context.Background(), conn)
	require.NoError(t, err)
	require.Len(t, inv.Devices, 3)
	byID := map[string]Device{}
	for _, d := range inv.Devices {
		byID[d.ID] = d
	}
	require.NotNil(t, byID["a"].Update)
	assert.Equal(t, "3", byID["a"].Update.Version, "the newest")
	assert.Nil(t, byID["b"].Update, "fwupd's NothingToDo")
	assert.Nil(t, byID["c"].Update)
	assert.NotContains(t, fake.asked(), "c", "a device that cannot be updated is not asked")

	require.Len(t, inv.History, 2)
	assert.Equal(t, HistoryEntry{DeviceID: "a", Name: "New", VersionOld: "1", Version: "2", State: "failed", Error: "no space", Time: 200}, inv.History[0])
	assert.Equal(t, "Old", inv.History[1].Name)
}

func TestSnapshotKeepsTheInventoryWhenUpdatesFail(t *testing.T) {
	fake := &fakeFwupd{
		devices:    []map[string]dbus.Variant{updatable("a", nil)},
		upgradeErr: dbus.NewError("org.freedesktop.fwupd.Internal", []interface{}{"boom"}),
	}
	conn, _ := busWithFwupd(t, fake, nil)
	inv, err := snapshot(context.Background(), conn)
	require.NoError(t, err)
	require.Len(t, inv.Devices, 1)
	assert.Nil(t, inv.Devices[0].Update)
}

func TestRefresh(t *testing.T) {
	metadata, signature := []byte("<components/>"), []byte("jcat")
	files := map[string][]byte{"/firmware.xml.zst": metadata, "/firmware.xml.zst.jcat": signature}
	remote := func(uri string, extra map[string]interface{}) map[string]dbus.Variant {
		d := map[string]interface{}{"RemoteId": "lvfs", "Enabled": true, "Type": uint32(remoteKindDownload), "Uri": uri + "/firmware.xml.zst"}
		for k, v := range extra {
			d[k] = v
		}
		return variants(d)
	}

	t.Run("fwupd gets the metadata and its signature", func(t *testing.T) {
		fake := &fakeFwupd{}
		conn, url := busWithFwupd(t, fake, files)
		fake.remotes = []map[string]dbus.Variant{remote(url, nil)}
		require.NoError(t, refresh(context.Background(), conn, 0))
		require.Len(t, fake.updated, 1)
		assert.Equal(t, updatedMetadata{id: "lvfs", data: metadata, signature: signature}, fake.updated[0])
	})

	t.Run("an unchanged signature is the end of it", func(t *testing.T) {
		fake := &fakeFwupd{}
		conn, url := busWithFwupd(t, fake, files)
		fake.remotes = []map[string]dbus.Variant{remote(url, map[string]interface{}{"Checksum": sum(signature)})}
		require.NoError(t, refresh(context.Background(), conn, 0))
		assert.Empty(t, fake.updated)
	})

	t.Run("disabled, local or recent remotes are left alone", func(t *testing.T) {
		fake := &fakeFwupd{}
		conn, url := busWithFwupd(t, fake, files)
		fake.remotes = []map[string]dbus.Variant{
			remote(url, map[string]interface{}{"Enabled": false}),
			remote(url, map[string]interface{}{"Type": uint32(2)}),
			remote(url, map[string]interface{}{"ModificationTime": uint64(time.Now().Unix())}),
		}
		require.NoError(t, refresh(context.Background(), conn, time.Hour))
		assert.Empty(t, fake.updated)
	})

	t.Run("0 ignores the age", func(t *testing.T) {
		fake := &fakeFwupd{}
		conn, url := busWithFwupd(t, fake, files)
		fake.remotes = []map[string]dbus.Variant{remote(url, map[string]interface{}{"ModificationTime": uint64(time.Now().Unix())})}
		require.NoError(t, refresh(context.Background(), conn, 0))
		assert.Len(t, fake.updated, 1)
	})

	t.Run("one remote failing does not stop the next", func(t *testing.T) {
		fake := &fakeFwupd{}
		conn, url := busWithFwupd(t, fake, files)
		fake.remotes = []map[string]dbus.Variant{
			variants(map[string]interface{}{"RemoteId": "gone", "Enabled": true, "Type": uint32(remoteKindDownload), "Uri": url + "/gone.xml"}),
			remote(url, nil),
		}
		require.Error(t, refresh(context.Background(), conn, 0))
		assert.Len(t, fake.updated, 1)
	})
}

func TestInstall(t *testing.T) {
	cab := []byte("the firmware")
	files := map[string][]byte{"/a.cab": cab}
	offered := func(url, checksum string) []map[string]dbus.Variant {
		return []map[string]dbus.Variant{release("2", map[string]interface{}{
			"Locations": []string{url + "/gone.cab", url + "/a.cab"},
			"Checksum":  checksum,
		})}
	}
	var mu sync.Mutex
	collect := func() (func(Progress), func() []Progress) {
		var seen []Progress
		return func(p Progress) {
				mu.Lock()
				defer mu.Unlock()
				seen = append(seen, p)
			}, func() []Progress {
				mu.Lock()
				defer mu.Unlock()
				return append([]Progress(nil), seen...)
			}
	}

	t.Run("the firmware reaches fwupd, from the next location that has it", func(t *testing.T) {
		fake := &fakeFwupd{devices: []map[string]dbus.Variant{updatable("a", nil)}, installTakes: 1500 * time.Millisecond}
		conn, url := busWithFwupd(t, fake, files)
		fake.upgrades = map[string][]map[string]dbus.Variant{"a": offered(url, sum(cab))}
		fake.afterInstall = []map[string]dbus.Variant{updatable("a", map[string]interface{}{"Flags": uint64(1<<1 | 1<<8)})}
		progress, seen := collect()

		result, err := install(context.Background(), conn, "a", "2", progress)
		require.NoError(t, err)
		assert.Equal(t, Result{Version: "2", NeedsReboot: true}, result)
		require.Len(t, fake.installed, 1)
		got := fake.installed[0]
		assert.Equal(t, "a", got.id)
		assert.Equal(t, cab, got.data)
		assert.Equal(t, "user-action", got.options["reason"].Value())
		assert.Equal(t, uint64(0), got.options["install-flags"].Value(), "no force, downgrade or checksum override")

		var followed bool
		for _, p := range seen() {
			if p.Stage == "install" && p.Status == "device-write" && p.Percent == 40 {
				followed = true
			}
		}
		assert.True(t, followed, "fwupd's status and percentage: %v", seen())
		entries, err := os.ReadDir(TempDir)
		require.NoError(t, err)
		assert.Empty(t, entries, "no download stays behind")
	})

	t.Run("no restart needed", func(t *testing.T) {
		fake := &fakeFwupd{devices: []map[string]dbus.Variant{updatable("a", nil)}}
		conn, url := busWithFwupd(t, fake, files)
		fake.upgrades = map[string][]map[string]dbus.Variant{"a": offered(url, sum(cab))}
		result, err := install(context.Background(), conn, "a", "2", func(Progress) {})
		require.NoError(t, err)
		assert.False(t, result.NeedsReboot)
	})

	t.Run("a version fwupd does not offer is refused without a download", func(t *testing.T) {
		fake := &fakeFwupd{devices: []map[string]dbus.Variant{updatable("a", nil)}}
		conn, url := busWithFwupd(t, fake, files)
		fake.upgrades = map[string][]map[string]dbus.Variant{"a": offered(url, sum(cab))}
		_, err := install(context.Background(), conn, "a", "9", func(Progress) {})
		require.ErrorIs(t, err, ErrNotOffered)
		assert.Empty(t, fake.installed)
	})

	t.Run("a wrong checksum never reaches fwupd", func(t *testing.T) {
		fake := &fakeFwupd{devices: []map[string]dbus.Variant{updatable("a", nil)}}
		conn, url := busWithFwupd(t, fake, files)
		fake.upgrades = map[string][]map[string]dbus.Variant{"a": offered(url, sum([]byte("other")))}
		_, err := install(context.Background(), conn, "a", "2", func(Progress) {})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "checksum")
		assert.Empty(t, fake.installed)
	})

	t.Run("no checksum, no install", func(t *testing.T) {
		fake := &fakeFwupd{devices: []map[string]dbus.Variant{updatable("a", nil)}}
		conn, url := busWithFwupd(t, fake, files)
		fake.upgrades = map[string][]map[string]dbus.Variant{"a": {release("2", map[string]interface{}{"Locations": []string{url + "/a.cab"}})}}
		_, err := install(context.Background(), conn, "a", "2", func(Progress) {})
		require.Error(t, err)
		assert.Empty(t, fake.installed)
	})

	t.Run("fwupd's refusal is the answer", func(t *testing.T) {
		fake := &fakeFwupd{
			devices:    []map[string]dbus.Variant{updatable("a", nil)},
			installErr: dbus.NewError("org.freedesktop.fwupd.BrokenSystem", []interface{}{"Secure boot is enabled, but shim isn't installed"}),
		}
		conn, url := busWithFwupd(t, fake, files)
		fake.upgrades = map[string][]map[string]dbus.Variant{"a": offered(url, sum(cab))}
		_, err := install(context.Background(), conn, "a", "2", func(Progress) {})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shim")
		var dbusErr dbus.Error
		assert.True(t, errors.As(err, &dbusErr))
	})
}

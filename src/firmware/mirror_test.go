package firmware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMirrorURL(t *testing.T) {
	for base, want := range map[string]string{
		"https://instance-registry.ironflock.com/dl": "https://instance-registry.ironflock.com/dl/lvfs",
		"http://10.0.0.2:15002/dl/":                  "http://10.0.0.2:15002/dl/lvfs",
		"":                                           "",
		"gs://bucket/dl":                             "",
		"not a url":                                  "",
	} {
		assert.Equal(t, want, MirrorURL(base), base)
	}
}

func TestSourceCheck(t *testing.T) {
	assert.NoError(t, source{url: "https://a/b"}.check())
	assert.Error(t, source{url: "http://a/b"}.check(), "plain http only for the update base")
	assert.NoError(t, source{url: "http://a/b", plain: true}.check())
	assert.Error(t, source{url: "ftp://a/b", plain: true}.check())
	assert.Error(t, source{url: "abc.cab"}.check(), "a bare name is no address")
}

func TestReleaseSources(t *testing.T) {
	const mirror = "http://appliance:15002/dl/lvfs"

	t.Run("the update base first, then the metadata's address", func(t *testing.T) {
		r := Release{Locations: []string{"https://fwupd.org/downloads/abc-a.cab"}}
		assert.Equal(t, []source{
			{url: mirror + "/abc-a.cab", plain: true},
			{url: "https://fwupd.org/downloads/abc-a.cab"},
		}, r.sources(mirror))
	})

	t.Run("a bare file name is relative to LVFS", func(t *testing.T) {
		r := Release{Locations: []string{"abc-a.cab"}}
		assert.Equal(t, []source{{url: "https://fwupd.org/downloads/abc-a.cab"}}, r.sources(""))
	})

	t.Run("no address, nothing to name", func(t *testing.T) {
		assert.Empty(t, (&Release{}).sources(mirror))
	})

	t.Run("the file name leaves query and path behind", func(t *testing.T) {
		assert.Equal(t, "abc-a.cab", (&Release{Locations: []string{"https://x/y/abc-a.cab?a=b#c"}}).fileName())
	})
}

func TestRefreshThroughTheMirror(t *testing.T) {
	direct, mirrored, jcat := []byte("from LVFS"), []byte("from the mirror"), []byte("jcat")
	files := map[string][]byte{
		"/firmware.xml.zst": direct, "/firmware.xml.zst.jcat": jcat,
		"/dl/lvfs/firmware.xml.zst": mirrored, "/dl/lvfs/firmware.xml.zst.jcat": jcat,
	}
	lvfs := func(url string) map[string]dbus.Variant {
		return variants(map[string]interface{}{"RemoteId": "lvfs", "Enabled": true, "Type": uint32(remoteKindDownload), "Uri": url + "/firmware.xml.zst"})
	}

	t.Run("the mirror comes first", func(t *testing.T) {
		fake := &fakeFwupd{}
		conn, url := busWithFwupd(t, fake, files)
		fake.setRemotes(lvfs(url))
		require.NoError(t, refresh(context.Background(), conn, 0, url+"/dl/lvfs"))
		require.Len(t, fake.metadata(), 1)
		assert.Equal(t, mirrored, fake.metadata()[0].data)
	})

	t.Run("an appliance without it falls back to LVFS", func(t *testing.T) {
		fake := &fakeFwupd{}
		conn, url := busWithFwupd(t, fake, files)
		fake.setRemotes(lvfs(url))
		require.NoError(t, refresh(context.Background(), conn, 0, url+"/old-appliance/lvfs"))
		require.Len(t, fake.metadata(), 1)
		assert.Equal(t, direct, fake.metadata()[0].data)
	})

	t.Run("what fwupd refuses from the mirror is not the end", func(t *testing.T) {
		fake := &fakeFwupd{refuse: mirrored}
		conn, url := busWithFwupd(t, fake, files)
		fake.setRemotes(lvfs(url))
		require.NoError(t, refresh(context.Background(), conn, 0, url+"/dl/lvfs"))
		require.Len(t, fake.metadata(), 1)
		assert.Equal(t, direct, fake.metadata()[0].data)
	})

	t.Run("only LVFS comes from the mirror", func(t *testing.T) {
		fake := &fakeFwupd{}
		conn, url := busWithFwupd(t, fake, files)
		fake.setRemotes(variants(map[string]interface{}{
			"RemoteId": "vendor", "Enabled": true, "Type": uint32(remoteKindDownload), "Uri": url + "/firmware.xml.zst",
		}))
		require.NoError(t, refresh(context.Background(), conn, 0, url+"/dl/lvfs"))
		require.Len(t, fake.metadata(), 1)
		assert.Equal(t, direct, fake.metadata()[0].data)
	})
}

func TestInstallThroughTheMirror(t *testing.T) {
	cab := []byte("the firmware")
	install2 := func(t *testing.T, fake *fakeFwupd, conn *dbus.Conn, mirror string, locations ...string) error {
		t.Helper()
		fake.setUpgrades(map[string][]map[string]dbus.Variant{"a": {release("2", map[string]interface{}{"Locations": locations, "Checksum": sum(cab)})}})
		_, err := install(context.Background(), conn, "a", "2", mirror, func(Progress) {})
		return err
	}

	t.Run("a bare file name from the mirror", func(t *testing.T) {
		fake := &fakeFwupd{devices: []map[string]dbus.Variant{updatable("a", nil)}}
		conn, url := busWithFwupd(t, fake, map[string][]byte{"/dl/lvfs/abc-a.cab": cab})
		require.NoError(t, install2(t, fake, conn, url+"/dl/lvfs", "abc-a.cab"))
		require.Len(t, fake.firmware(), 1)
	})

	t.Run("a bad copy on the mirror falls through to the address", func(t *testing.T) {
		fake := &fakeFwupd{devices: []map[string]dbus.Variant{updatable("a", nil)}}
		conn, url := busWithFwupd(t, fake, map[string][]byte{"/dl/lvfs/abc-a.cab": []byte("tampered"), "/abc-a.cab": cab})
		require.NoError(t, install2(t, fake, conn, url+"/dl/lvfs", url+"/abc-a.cab"))
		require.Len(t, fake.firmware(), 1)
		assert.Equal(t, cab, fake.firmware()[0].data)
	})

	t.Run("a bad copy and nothing else never reaches fwupd", func(t *testing.T) {
		fake := &fakeFwupd{devices: []map[string]dbus.Variant{updatable("a", nil)}}
		conn, url := busWithFwupd(t, fake, map[string][]byte{"/dl/lvfs/abc-a.cab": []byte("tampered")})
		err := install2(t, fake, conn, url+"/dl/lvfs", url+"/abc-a.cab")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "checksum")
		assert.Empty(t, fake.firmware())
	})

	t.Run("an appliance without a domain serves plain http", func(t *testing.T) {
		fake := &fakeFwupd{devices: []map[string]dbus.Variant{updatable("a", nil)}}
		conn, _ := busWithFwupd(t, fake, nil)
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(cab) }))
		t.Cleanup(plain.Close)
		require.NoError(t, install2(t, fake, conn, plain.URL+"/dl/lvfs", "abc-a.cab"))
		require.Len(t, fake.firmware(), 1)
	})
}

func TestAnnounce(t *testing.T) {
	var mu sync.Mutex
	var heads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		heads = append(heads, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	inv := Inventory{Devices: []Device{
		{ID: "a", Update: &Release{Locations: []string{"abc-a.cab"}}},
		{ID: "b", Update: &Release{Locations: []string{"https://fwupd.org/downloads/abc-a.cab"}}},
		{ID: "c", Update: &Release{Locations: []string{"def-c.cab"}}},
		{ID: "d"},
	}}

	Announce(context.Background(), srv.URL+"/dl/lvfs", inv)
	assert.Equal(t, []string{"HEAD /dl/lvfs/abc-a.cab", "HEAD /dl/lvfs/def-c.cab"}, heads, "each file once")

	t.Run("without a mirror nothing is sent", func(t *testing.T) {
		heads = nil
		Announce(context.Background(), "", inv)
		assert.Empty(t, heads)
	})

	t.Run("a mirror that is away is not an error", func(t *testing.T) {
		srv.Close()
		Announce(context.Background(), srv.URL+"/dl/lvfs", inv)
	})
}

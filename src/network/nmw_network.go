package network

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"reagent/errdefs"
	"reagent/networkmanager"
	"reagent/safe"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/rs/zerolog/log"
)

type NWMNetwork struct {
	nm       networkmanager.NetworkManager
	settings networkmanager.Settings
}

func NewNMWNetwork() (NWMNetwork, error) {
	nm, err := networkmanager.NewNetworkManager()
	if err != nil {
		return NWMNetwork{}, err
	}

	settings, err := networkmanager.NewSettings()
	if err != nil {
		return NWMNetwork{}, err
	}

	return NWMNetwork{nm, settings}, nil
}

func (n NWMNetwork) getWirelessDevice() (networkmanager.DeviceWireless, error) {
	activeWirelessDevice, err := n.getActiveWirelessDevice()
	if err == nil {
		return activeWirelessDevice, nil
	}

	devices, err := n.nm.GetAllDevices()
	if err != nil {
		return nil, err
	}

	for _, device := range devices {
		devType, err := device.GetPropertyDeviceType()
		if err != nil {
			return nil, err
		}

		if devType == networkmanager.NmDeviceTypeWifi {
			return networkmanager.NewDeviceWireless(device.GetPath())
		}
	}

	return nil, ErrDeviceNotFound
}

func (n NWMNetwork) getActiveWirelessDevice() (networkmanager.DeviceWireless, error) {
	connections, err := n.nm.GetPropertyActiveConnections()
	if err != nil {
		return nil, err
	}

	for _, connection := range connections {
		devices, err := connection.GetPropertyDevices()
		if err != nil {
			return nil, err
		}

		for _, device := range devices {
			devType, err := device.GetPropertyDeviceType()
			if err != nil {
				return nil, err
			}

			if devType == networkmanager.NmDeviceTypeWifi {
				return networkmanager.NewDeviceWireless(device.GetPath())
			}
		}

	}

	return nil, ErrDeviceNotFound
}

func (n NWMNetwork) getAccessPointByMAC(mac string) (networkmanager.AccessPoint, error) {
	activeWirelessDevice, err := n.getWirelessDevice()
	if err != nil {
		return nil, err
	}

	accessPoints, err := activeWirelessDevice.GetAllAccessPoints()
	if err != nil {
		return nil, err
	}

	for _, ap := range accessPoints {
		apMAC, err := ap.GetPropertyHWAddress()
		if err != nil {
			return nil, err
		}

		if apMAC == mac {
			return ap, nil
		}
	}

	return nil, errdefs.ErrNotFound
}

func (n NWMNetwork) Reload() error {
	return n.nm.Reload(0)
}

// SetInfiniteAutoconnectRetries forces autoconnect-retries to 0 (infinite) on every
// ethernet and wifi connection profile. By default NetworkManager gives up activating
// a connection after 4 failed attempts, which can leave a headless device permanently
// offline after a transient DHCP/router outage (e.g. a router reboot on a home network)
// until it is manually rebooted. Failures on individual connections are logged and
// skipped so one bad profile can't block the rest.
func (n NWMNetwork) SetInfiniteAutoconnectRetries() error {
	connections, err := n.settings.ListConnections()
	if err != nil {
		return err
	}

	for _, connection := range connections {
		settings, err := connection.GetSettings()
		if err != nil {
			continue
		}

		connSettings := settings["connection"]
		if connSettings == nil {
			continue
		}

		connType := fmt.Sprint(connSettings["type"])
		if connType != "802-3-ethernet" && connType != "802-11-wireless" {
			continue
		}

		// already infinite, nothing to do
		if retries, ok := connSettings["autoconnect-retries"].(int32); ok && retries == 0 {
			continue
		}

		connSettings["autoconnect-retries"] = int32(0)

		if err := connection.Update(settings); err != nil {
			log.Error().Err(err).Msgf("failed to set autoconnect-retries on connection %v", connSettings["id"])
			continue
		}
	}

	return nil
}

func (n NWMNetwork) getConnectionBySSID(ssid string) (networkmanager.Connection, error) {
	savedConnections, err := n.settings.ListConnections()
	if err != nil {
		return nil, err
	}

	for _, connection := range savedConnections {
		settingsMap, err := connection.GetSettings()
		if err != nil {
			return nil, err
		}

		settings := settingsMap["802-11-wireless"]
		ssidArg := settings["ssid"]

		if ssidArg == nil {
			continue
		}

		ssidByteArr, ok := ssidArg.([]byte)
		if !ok {
			return nil, errors.New("failed to parse ssid")
		}

		foundSSID := string(ssidByteArr)

		if ssid == foundSSID {
			return connection, nil
		}
	}

	return nil, errdefs.ErrNotFound
}

func (n NWMNetwork) RemoveWifi(ssid string) error {
	connection, err := n.getConnectionBySSID(ssid)
	if err != nil {
		return err
	}

	return connection.Delete()
}

func (n NWMNetwork) ActivateWiFi(mac string, ssid string) error {
	device, err := n.getWirelessDevice()
	if err != nil {
		return err
	}

	accessPoint, err := n.getAccessPointByMAC(mac)
	if err != nil {
		return err
	}

	connection, err := n.getConnectionBySSID(ssid)
	if err != nil {
		return err
	}

	_, err = n.nm.ActivateWirelessConnection(connection, device, accessPoint)
	if err != nil {
		return err
	}

	return nil
}

func (n NWMNetwork) AddWiFi(mac string, credentials WiFiCredentials) error {
	var ap networkmanager.AccessPoint
	var ssid string
	var secType networkmanager.AccessPointSecurityType
	var err error

	if mac == "" {
		ssid = credentials.Ssid
		secType = networkmanager.AccessPointSecurityType(credentials.SecurityType)
	} else {
		ap, err = n.getAccessPointByMAC(mac)
		if err != nil {
			return err
		}

		ssid, err = ap.GetPropertySSID()
		if err != nil {
			return err
		}

		secType, err = ap.GetSecurityType()
		if err != nil {
			return err
		}
	}

	// see https://developer.gnome.org/NetworkManager/stable/settings-802-11-wireless-security.html
	newConnection := make(networkmanager.ConnectionSettings)
	newConnection["connection"] = make(map[string]interface{})
	newConnection["connection"]["id"] = ssid
	newConnection["connection"]["autoconnect-priority"] = credentials.Priority
	// never give up reconnecting after transient outages (default would stop after 4 tries)
	newConnection["connection"]["autoconnect-retries"] = int32(0)

	newConnection["802-11-wireless"] = make(map[string]interface{})
	newConnection["802-11-wireless"]["ssid"] = []byte(ssid)

	newConnection["802-11-wireless"]["security"] = "802-11-wireless-security"
	newConnection["802-11-wireless-security"] = make(map[string]interface{})
	newConnection["802-11-wireless-security"]["key-mgmt"] = secType

	if secType == networkmanager.AccessPointSecurityWPA || secType == networkmanager.AccessPointSecurityWPAEnterprise {
		newConnection["802-11-wireless-security"]["psk-flags"] = 0 // Default
		newConnection["802-11-wireless-security"]["psk"] = credentials.Passwd
	} else if secType == networkmanager.AccessPointSecurityWEP {
		newConnection["802-11-wireless-security"]["wep-key-type"] = 1 // NM_WEP_KEY_TYPE_KEY
		newConnection["802-11-wireless-security"]["wep-key0"] = credentials.Passwd
	}

	_, err = n.settings.AddConnection(newConnection)
	if err != nil {
		if strings.Contains(err.Error(), "802-11-wireless-security.psk: property is invalid") {
			return ErrInvalidWiFiPassword
		}
		if strings.Contains(err.Error(), "802-11-wireless-security.wep-key0: property is invalid") {
			return ErrInvalidWiFiPassword
		}
		return err
	}

	return nil
}

// Scan calls the NetworkManager to see if there's a WiFi device available. If available it will scan, and block until the scan has finished.
// Whenever a scan request gets rate limited, we wait one second and retry until it succeeds. The default scan timeout is 10 seconds.
func (n NWMNetwork) Scan(timeoutParam ...time.Duration) error {
	var timeout time.Duration

	if len(timeoutParam) == 0 {
		timeout = time.Second * 10
	} else {
		timeout = timeoutParam[0]
	}

	ctx, cancelFunc := context.WithTimeout(context.Background(), timeout)
	defer cancelFunc()

	wirelessDevice, err := n.getWirelessDevice()
	if err != nil {
		return err
	}

	lastScanBeforeScan, err := wirelessDevice.GetPropertyLastScan()
	if err != nil {
		return err
	}

	err = wirelessDevice.RequestScan()
	if err != nil {
		// we got rate limited, lets retry in one second
		if errors.Is(err, networkmanager.ErrorFollowingPreviousScan) {
			// wait 1 second and then retry the scan
			time.Sleep(time.Second)

			return n.Scan(timeout)
		}
	}

outerLoop:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			lastScan, err := wirelessDevice.GetPropertyLastScan()
			if err != nil {
				return err
			}

			// scan has completed
			if lastScanBeforeScan != lastScan {
				break outerLoop
			}

			devState, err := wirelessDevice.GetPropertyState()
			if err != nil {
				return err
			}

			// the scan won't complete if the device is not activated / managed
			if devState <= networkmanager.NmDeviceStateDisconnected {
				return ErrNotConnected
			}

			time.Sleep(time.Millisecond * 100)
		}
	}

	return nil
}

func (n NWMNetwork) isKnown(ssid string, bssid string) (bool, error) {
	savedConnections, err := n.settings.ListConnections()
	if err != nil {
		return false, err
	}

	var allBSSIDs []string
	var allSSIDs []string
	for _, connection := range savedConnections {
		settingsMap, err := connection.GetSettings()
		if err != nil {
			return false, err
		}

		settings := settingsMap["802-11-wireless"]
		ssidArg := settings["ssid"]
		seenBSSIDs := settings["seen-bssids"]

		if ssidArg != nil {
			ssid, ok := ssidArg.([]byte)
			if !ok {
				return false, errors.New("failed to parse ssid")
			}

			allSSIDs = append(allSSIDs, string(ssid))
		}

		// hasn't been actually connected to an AP yet
		if seenBSSIDs == nil {
			continue
		}

		bssids, ok := seenBSSIDs.([]string)
		if !ok {
			return false, nil
		}

		allBSSIDs = append(allBSSIDs, bssids...)
	}

	for _, knownBSSID := range allBSSIDs {
		if knownBSSID == bssid {
			return true, nil
		}
	}

	for _, knownSSID := range allSSIDs {
		if knownSSID == ssid {
			return true, nil
		}
	}

	return false, nil
}

func (n NWMNetwork) isCurrent(bssid string) (bool, error) {
	activeWirelessDevice, err := n.getWirelessDevice()
	if err != nil {
		return false, err
	}

	ac, err := activeWirelessDevice.GetPropertyActiveConnection()
	if err != nil {
		return false, err
	}

	if ac == nil {
		return false, nil
	}

	connection, err := ac.GetPropertyConnection()
	if err != nil {
		return false, err
	}

	settingsMap, err := connection.GetSettings()
	if err != nil {
		return false, err
	}

	settings := settingsMap["802-11-wireless"]
	bssids, ok := settings["seen-bssids"].([]string)
	if !ok {
		return false, nil
	}

	for _, currentBssid := range bssids {
		if currentBssid == bssid {
			return true, nil
		}
	}

	return false, nil
}

func (n NWMNetwork) getAllConnectionSettings() ([]networkmanager.ConnectionSettings, error) {
	var connectionSettings []networkmanager.ConnectionSettings

	allConnections, err := n.settings.ListConnections()
	if err != nil {
		return nil, err
	}

	for _, connection := range allConnections {
		setting, err := connection.GetSettings()
		if err != nil {
			continue
		}

		connectionSettings = append(connectionSettings, setting)
	}

	return connectionSettings, nil
}

func (n NWMNetwork) getEthernetDevices() ([]networkmanager.DeviceWired, error) {
	devices, err := n.nm.GetAllDevices()
	if err != nil {
		return nil, err
	}

	var wiredDevices []networkmanager.DeviceWired
	for _, device := range devices {
		devType, err := device.GetPropertyDeviceType()
		if err != nil {
			return nil, err
		}

		if devType == networkmanager.NmDeviceTypeEthernet {
			wiredDevice, err := networkmanager.NewDeviceWired(device.GetPath())
			if err != nil {
				return nil, err
			}

			wiredDevices = append(wiredDevices, wiredDevice)
		}
	}

	return wiredDevices, nil
}

func (n NWMNetwork) accessPointsToWiFi(accessPoints []networkmanager.AccessPoint) ([]WiFi, error) {
	var wifis []WiFi
	for _, ap := range accessPoints {
		wifi := WiFi{}

		ssid, err := ap.GetPropertySSID()
		if err != nil {
			ssid = ""
		}

		wifi.SSID = ssid

		mac, err := ap.GetPropertyHWAddress()
		if err != nil {
			return nil, err
		}

		wifi.MAC = mac

		known, err := n.isKnown(ssid, mac)
		if err != nil {
			return nil, err
		}

		wifi.Known = known

		current, err := n.isCurrent(mac)
		if err != nil {
			return nil, err
		}

		wifi.Current = current

		wpaFlags, err := ap.GetPropertyWPAFlags()
		if err != nil {
			return nil, err
		}

		wifi.WPAFlags = wpaFlags

		signalStrength, err := ap.GetPropertyStrength()
		if err != nil {
			return nil, err
		}

		wifi.Signal = signalStrength

		flags, err := ap.GetPropertyFlags()
		if err != nil {
			return nil, err
		}

		wifi.Flags = flags

		freq, err := ap.GetPropertyFrequency()
		if err != nil {
			return nil, err
		}

		wifi.Frequency = freq

		channel, err := ap.GetChannel()
		if err != nil {
			return nil, err
		}

		wifi.Channel = channel

		rsnFlags, err := ap.GetPropertyRSNFlags()
		if err != nil {
			return nil, err
		}

		wifi.RSNFlags = rsnFlags

		securityType, err := ap.GetSecurityType()
		if err != nil {
			return nil, err
		}

		wifi.SecurityType = string(securityType)

		wifis = append(wifis, wifi)
	}

	return wifis, nil
}

func (n NWMNetwork) GetActiveWirelessDeviceConfig() ([]IPv4AddressData, []IPv6AddressData, error) {
	device, err := n.getWirelessDevice()
	if err != nil {
		return nil, nil, err
	}

	ip4Config, err := device.GetPropertyIP4Config()
	if err != nil {
		return nil, nil, err
	}

	ip6Config, err := device.GetPropertyIP6Config()
	if err != nil {
		return nil, nil, err
	}

	var parsedIPv4Datas []IPv4AddressData
	if ip4Config != nil {
		ipv4AddressDatas, err := ip4Config.GetPropertyAddressData()
		if err != nil {
			return nil, nil, err
		}

		for _, addressData := range ipv4AddressDatas {
			parsedIPv4Datas = append(parsedIPv4Datas, IPv4AddressData(addressData))
		}
	}

	var parsedIPv6Datas []IPv6AddressData
	if ip6Config != nil {
		ipv6AddressDatas, err := ip6Config.GetPropertyAddressData()
		if err != nil {
			return nil, nil, err
		}

		for _, addressData := range ipv6AddressDatas {
			parsedIPv6Datas = append(parsedIPv6Datas, IPv6AddressData(addressData))
		}
	}

	return parsedIPv4Datas, parsedIPv6Datas, nil
}

func swapUint32(n uint32) uint32 {
	return (n&0x000000FF)<<24 | (n&0x0000FF00)<<8 |
		(n&0x00FF0000)>>8 | (n&0xFF000000)>>24
}

func ip2Long(ip string) uint32 {
	var long uint32
	_ = binary.Read(bytes.NewBuffer(net.ParseIP(ip).To4()), binary.BigEndian, &long)
	return swapUint32(long)
}

func (n NWMNetwork) updateIPv4Address(device networkmanager.Device, connection networkmanager.Connection, ipAddress string, prefix uint32) error {
	settings, err := connection.GetSettings()
	if err != nil {
		return err
	}

	delete(settings["ipv6"], "addresses")
	delete(settings["ipv6"], "routes")

	if settings["ipv4"] == nil {
		settings["ipv4"] = make(map[string]interface{})
	}

	if settings["ipv4"]["addresses"] != nil {
		delete(settings["ipv4"], "addresses")
	}

	if settings["ipv4"]["address-data"] != nil {
		delete(settings["ipv4"], "address-data")
	}

	if settings["ipv4"]["gateway"] != nil {
		delete(settings["ipv4"], "gateway")
	}

	if settings["ipv4"]["dns"] != nil {
		delete(settings["ipv4"], "dns")
	}

	devSettings, err := device.GetPropertyIP4Config()
	if err != nil {
		return err
	}

	if devSettings != nil {
		gateway, err := devSettings.GetPropertyGateway()
		if err != nil {
			return err
		}

		dnsStrings, err := devSettings.GetPropertyNameservers()
		if err != nil {
			return err
		}

		dnsInts := make([]uint32, len(dnsStrings))
		for i, dnsString := range dnsStrings {
			dnsInts[i] = ip2Long(dnsString)
		}

		settings["ipv4"]["gateway"] = gateway
		settings["ipv4"]["dns"] = dnsInts
	}

	settings["ipv4"]["method"] = "manual"
	addressData := make([]map[string]interface{}, 1)
	addressData[0] = make(map[string]interface{})
	addressData[0]["address"] = ipAddress
	addressData[0]["prefix"] = prefix

	settings["ipv4"]["address-data"] = addressData

	err = connection.Update(settings)
	if err != nil {
		return err
	}

	if device != nil {
		safe.Go(func() {
			_, err = n.nm.ActivateConnection(connection, device, "/")
			if err != nil {
				log.Error().Err(err).Msgf("Error while activating connection: %s", err)
			}
		})
	}

	return err
}

func (n NWMNetwork) enableDHCP(device networkmanager.Device, connection networkmanager.Connection) error {
	settings, err := connection.GetSettings()
	if err != nil {
		return err
	}

	delete(settings["ipv6"], "addresses")
	delete(settings["ipv6"], "routes")

	if settings["ipv4"] == nil {
		settings["ipv4"] = make(map[string]interface{})
	}

	if settings["ipv4"]["addresses"] != nil {
		delete(settings["ipv4"], "addresses")
	}

	if settings["ipv4"]["address-data"] != nil {
		delete(settings["ipv4"], "address-data")
	}

	if settings["ipv4"]["gateway"] != nil {
		delete(settings["ipv4"], "gateway")
	}

	settings["ipv4"]["method"] = "auto"

	err = connection.Update(settings)
	if err != nil {
		return err
	}

	if device != nil {
		safe.Go(func() {
			_, err = n.nm.ActivateConnection(connection, device, "/")
			if err != nil {
				log.Error().Err(err).Msgf("Error while activating connection: %s", err)
			}
		})
	}

	return err
}

func (n NWMNetwork) EnableDHCP(mac string, interfaceName string) error {
	devices, err := n.nm.GetAllDevices()
	if err != nil {
		return err
	}

	var ac networkmanager.ActiveConnection
	var foundDevice networkmanager.Device
	for _, device := range devices {
		foundMac, err := device.GetPropertyHwAddress()
		if err != nil {
			return err
		}

		if foundMac == mac {
			ac, err = device.GetPropertyActiveConnection()
			if err != nil {
				return err
			}

			foundDevice = device
			break
		}

	}

	var foundConnection networkmanager.Connection
	if ac != nil {
		foundConnection, err = ac.GetPropertyConnection()
	} else {
		foundConnection, err = n.getConnectionByInterfaceName(interfaceName)
	}

	if err != nil {
		return err
	}

	return n.enableDHCP(foundDevice, foundConnection)
}

func (n NWMNetwork) getConnectionByInterfaceName(interfaceName string) (networkmanager.Connection, error) {
	connections, err := n.settings.ListConnections()
	if err != nil {
		return nil, err
	}

	for _, connection := range connections {
		cSettings, err := connection.GetSettings()
		if err != nil {
			return nil, err
		}

		foundInterfaceName := fmt.Sprint(cSettings["connection"]["interface-name"])
		if interfaceName == foundInterfaceName {
			return connection, nil
		}
	}

	return nil, errdefs.ErrNotFound
}

func (n NWMNetwork) SetIPv4Address(mac string, interfaceName string, ip string, prefix uint32) error {
	devices, err := n.nm.GetAllDevices()
	if err != nil {
		return err
	}

	var ac networkmanager.ActiveConnection
	var foundDevice networkmanager.Device
	for _, device := range devices {
		foundMac, err := device.GetPropertyHwAddress()
		if err != nil {
			return err
		}

		if foundMac == mac {
			ac, err = device.GetPropertyActiveConnection()
			if err != nil {
				return err
			}

			foundDevice = device
			break
		}

	}

	var foundConnection networkmanager.Connection
	if ac != nil {
		foundConnection, err = ac.GetPropertyConnection()
	} else {
		foundConnection, err = n.getConnectionByInterfaceName(interfaceName)
	}

	if err != nil {
		return err
	}

	return n.updateIPv4Address(foundDevice, foundConnection, ip, prefix)
}

func (n NWMNetwork) connectionSettingsToWifi(connectionSettingsMap networkmanager.ConnectionSettings) WiFi {
	var wifi WiFi

	wirelessSettings := connectionSettingsMap["802-11-wireless"]
	wirelessSecurity := connectionSettingsMap["802-11-wireless-security"]
	securityType := fmt.Sprint(wirelessSecurity["key-mgmt"])

	ssidByteArr, ok := wirelessSettings["ssid"].([]byte)
	if !ok {
		ssidByteArr = []byte{}
	}

	ssid := string(ssidByteArr)

	wifi.SSID = ssid
	wifi.Known = true
	wifi.SecurityType = securityType

	return wifi
}

func (n NWMNetwork) ListEthernetDevices() ([]EthernetDevice, error) {
	devices, err := n.getEthernetDevices()
	if err != nil {
		return nil, err
	}

	var ethernetDevices []EthernetDevice
	for _, device := range devices {
		var ethernetDevice EthernetDevice
		name, err := device.GetPropertyIpInterface()
		if err != nil {
			return nil, err
		}

		if name == "" {
			name, err = device.GetPropertyInterface()
			if err != nil {
				return nil, err
			}
		}

		ethernetDevice.InterfaceName = name

		ipv4Config, err := device.GetPropertyIP4Config()
		if err != nil {
			return nil, err
		}

		if ipv4Config != nil {
			ipv4AddressDatas, err := ipv4Config.GetPropertyAddressData()
			if err != nil {
				return nil, err
			}

			var parsedIPv4AddressDatas []IPv4AddressData
			for _, ipv4AddressData := range ipv4AddressDatas {
				parsedIPv4AddressDatas = append(parsedIPv4AddressDatas, IPv4AddressData(ipv4AddressData))
			}

			ethernetDevice.IPv4AddressData = parsedIPv4AddressDatas
		} else {
			connection, _ := n.getConnectionByInterfaceName(ethernetDevice.InterfaceName)

			if connection != nil {
				settings, err := connection.GetSettings()
				if err != nil {
					return nil, err
				}

				if settings["ipv4"] != nil && settings["ipv4"]["address-data"] != nil {
					ipv4Addresses := settings["ipv4"]["address-data"].([]map[string]dbus.Variant)
					var ipv4AddressDatas []IPv4AddressData
					for _, addressData := range ipv4Addresses {
						ipv4Address := fmt.Sprint(addressData["address"].Value())
						prefix := addressData["prefix"].Value().(uint32)
						ipv4AddressDatas = append(ipv4AddressDatas, IPv4AddressData{Prefix: uint8(prefix), Address: ipv4Address})
					}

					ethernetDevice.IPv4AddressData = ipv4AddressDatas
				}
			}
		}

		ipv6Config, err := device.GetPropertyIP6Config()
		if err != nil {
			return nil, err
		}

		if ipv6Config != nil {
			ipv6AddressDatas, err := ipv6Config.GetPropertyAddressData()
			if err != nil {
				return nil, err
			}

			var parsedIPv6AddressDatas []IPv6AddressData
			for _, ipv6AddressData := range ipv6AddressDatas {
				parsedIPv6AddressDatas = append(parsedIPv6AddressDatas, IPv6AddressData(ipv6AddressData))
			}

			ethernetDevice.IPv6AddressData = parsedIPv6AddressDatas
		}

		mac, err := device.GetPropertyPermHwAddress()
		if err != nil {
			return nil, err
		}

		ethernetDevice.MAC = mac

		ac, err := device.GetPropertyActiveConnection()
		if err != nil {
			return nil, err
		}

		if ac != nil {
			conn, err := ac.GetPropertyConnection()
			if err != nil {
				return nil, err
			}

			if conn != nil {
				settings, err := conn.GetSettings()
				if err != nil {
					return nil, err
				}

				if settings["ipv4"] != nil {
					method := settings["ipv4"]["method"]
					if method != nil {
						ethernetDevice.Method = fmt.Sprint(method)
					}
				}
			}
		} else {
			if ethernetDevice.InterfaceName == "" {
				continue
			}

			connection, _ := n.getConnectionByInterfaceName(ethernetDevice.InterfaceName)

			if connection != nil {
				settings, err := connection.GetSettings()
				if err != nil {
					return nil, err
				}

				if settings["ipv4"] != nil {
					method := settings["ipv4"]["method"]
					if method != nil {
						ethernetDevice.Method = fmt.Sprint(method)
					}
				}
			}
		}

		ethernetDevices = append(ethernetDevices, ethernetDevice)
	}

	return ethernetDevices, nil
}

func (n NWMNetwork) ListWifiNetworks() ([]WiFi, error) {
	activeWirelessDevice, err := n.getWirelessDevice()
	if err != nil {
		return nil, err
	}

	accessPoints, err := activeWirelessDevice.GetAllAccessPoints()
	if err != nil {
		return nil, err
	}

	wifis, err := n.accessPointsToWiFi(accessPoints)
	if err != nil {
		return nil, err
	}

	allConnectionSettings, err := n.getAllConnectionSettings()
	if err != nil {
		return nil, err
	}

	for _, connectionSettingsMap := range allConnectionSettings {
		settings := connectionSettingsMap["802-11-wireless"]
		ssidByteArr, ok := settings["ssid"].([]byte)
		if !ok {
			continue
		}

		ssid := string(ssidByteArr)

		found := false
		for _, wifi := range wifis {
			if wifi.SSID == ssid {
				found = true
			}
		}

		if !found {
			wifis = append(wifis, n.connectionSettingsToWifi(connectionSettingsMap))
		}
	}

	return wifis, nil
}

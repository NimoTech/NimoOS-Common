package model

// NetworkInterface represents a single network interface configuration.
// Type is auto-detected from sysfs: "ethernet", "wifi", "bridge", "thunderbolt".
type NetworkInterface struct {
	Name          string          `json:"name"`
	Type          string          `json:"type"`       // "ethernet", "bridge", "wifi", "thunderbolt"
	IsVirtual     bool            `json:"is_virtual"` // true for br-*, docker0, etc.
	MAC           string          `json:"mac"`
	Speed         string          `json:"speed,omitempty"` // e.g. "1000", "20000"
	State         string          `json:"state"`           // "up", "down"
	IPv4          *IPv4Config     `json:"ipv4,omitempty"`
	Wireless      *WirelessConfig `json:"wireless,omitempty"`
	Zone          string          `json:"zone,omitempty"`          // "lan", "wan", or ""
	Ports         []string        `json:"ports,omitempty"`         // for bridges, which ports are attached
	HybridCapable bool            `json:"hybridCapable,omitempty"` // WiFi card supports client+AP simultaneously
}

type IPv4Config struct {
	Method  string   `json:"method"` // "static", "dhcp", "manual"
	Address string   `json:"address,omitempty"`
	Netmask string   `json:"netmask,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
	DNS     []string `json:"dns,omitempty"`
}

type WirelessConfig struct {
	Mode       string `json:"mode"` // "client", "ap", "concurrent"
	SSID       string `json:"ssid,omitempty"`
	ApSsid     string `json:"apSsid,omitempty"`
	Password   string `json:"password,omitempty"`   // client password (wpa-psk)
	ApPassword string `json:"apPassword,omitempty"` // AP/hotspot password
	Channel    int    `json:"channel,omitempty"`
	HybridMode bool   `json:"hybridMode,omitempty"`
}

// ClientConfig is the unified client config for a single interface (stored in network-config.json)
type ClientConfig struct {
	SSID     string `json:"ssid,omitempty"`
	Password string `json:"password,omitempty"`
}

// HotspotConfig is the unified AP config for a single interface (stored in network-config.json)
type HotspotConfig struct {
	SSID     string `json:"ssid"`
	Password string `json:"password,omitempty"`
	Channel  int    `json:"channel,omitempty"`
}

// UnifiedInterfaceConfig is the single-source-of-truth for a network interface.
// One JSON file replaces the 3-4 separate config files.
type UnifiedInterfaceConfig struct {
	Mode    string          `json:"mode"`             // "client", "ap", "concurrent"
	Zone    string          `json:"zone,omitempty"`   // "lan", "wan", ""
	Client  *ClientConfig   `json:"client,omitempty"`
	Hotspot *HotspotConfig  `json:"hotspot,omitempty"`
	IPv4    *IPv4Config     `json:"ipv4,omitempty"`
}

type NetworkZone struct {
	Name       string   `json:"name"`
	Interfaces []string `json:"interfaces"`
	// Advanced routing/firewall rules will go here in Phase 5
}

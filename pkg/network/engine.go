package network

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/NimoTech/NimoOS-Common/model"
)

const (
	InterfacesPath    = "/etc/network/interfaces"
	InterfacesDPath   = "/etc/network/interfaces.d"
	NimoosConfDir     = "/etc/nimoos"
	NetworkConfigFile = "/etc/nimoos/network-config.json"
	// Legacy file paths — kept for backward compat migration
	ZoneConfigFile     = "/etc/nimoos/network-zones.json"
	ApConfigFile       = "/etc/nimoos/network-ap.json"
	WifiModeConfigFile = "/etc/nimoos/network-wifi-mode.json"
)

var ifaceLineRe = regexp.MustCompile(`^iface\s+(\S+)\s+inet\s+(\S+)`)

// ── Unified Config (Single Source of Truth) ──────────────────────────────────

// loadUnifiedConfig reads the unified network-config.json.
// If the file doesn't exist, it migrates from legacy files.
func loadUnifiedConfig() map[string]model.UnifiedInterfaceConfig {
	data, err := os.ReadFile(NetworkConfigFile)
	if err != nil {
		return migrateFromOldConfig()
	}
	var cfgs map[string]model.UnifiedInterfaceConfig
	if json.Unmarshal(data, &cfgs) != nil {
		return migrateFromOldConfig()
	}
	return cfgs
}

func saveUnifiedConfig(cfgs map[string]model.UnifiedInterfaceConfig) error {
	if err := os.MkdirAll(NimoosConfDir, 0755); err != nil {
		return fmt.Errorf("failed to create %s: %w", NimoosConfDir, err)
	}
	data, err := json.MarshalIndent(cfgs, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal network config: %w", err)
	}
	return os.WriteFile(NetworkConfigFile, data, 0644)
}

// migrateFromOldConfig reads legacy files and creates the unified config.
func migrateFromOldConfig() map[string]model.UnifiedInterfaceConfig {
	result := make(map[string]model.UnifiedInterfaceConfig)

	// Parse interfaces.d/ files for client/IPv4 configs
	oldIfaces := parseAllConfigsFromDisk()
	for _, old := range oldIfaces {
		if old.Name == "lo" || old.Name == "" {
			continue
		}
		uc := model.UnifiedInterfaceConfig{
			Mode: "manual",
			Zone: loadZones()[old.Name],
		}
		if old.IPv4 != nil {
			uc.IPv4 = old.IPv4
		}
		if old.Wireless != nil && old.Wireless.SSID != "" {
			uc.Mode = "client"
			uc.Client = &model.ClientConfig{
				SSID:     old.Wireless.SSID,
				Password: old.Wireless.Password,
			}
		}
		result[old.Name] = uc
	}

	// Merge AP configs
	for name, ap := range loadApConfigs() {
		uc, ok := result[name]
		if !ok {
			uc = model.UnifiedInterfaceConfig{Mode: "ap"}
		}
		if uc.Mode == "client" {
			uc.Mode = "concurrent"
		} else if uc.Mode != "concurrent" {
			uc.Mode = "ap"
		}
		uc.Hotspot = &model.HotspotConfig{
			SSID:     ap.ApSsid,
			Password: ap.Password,
			Channel:  ap.Channel,
		}
		if uc.IPv4 == nil {
			uc.IPv4 = &model.IPv4Config{Method: "static", Address: "192.168.22.1", Netmask: "255.255.255.0"}
		}
		result[name] = uc
	}

	// Apply stored wifi mode override (newest source of truth)
	for name, mode := range loadWifiModes() {
		uc, ok := result[name]
		if !ok {
			uc = model.UnifiedInterfaceConfig{Mode: mode}
			result[name] = uc
			continue
		}
		uc.Mode = mode
		if mode == "ap" {
			uc.Client = nil
		} else if mode == "client" {
			uc.Hotspot = nil
		}
		result[name] = uc
	}

	// Save the unified config for future reads
	_ = saveUnifiedConfig(result)
	return result
}

// parseAllConfigsFromDisk reads interfaces.d/ files and returns flat list.
func parseAllConfigsFromDisk() []model.NetworkInterface {
	var results []model.NetworkInterface

	data, err := os.ReadFile(InterfacesPath)
	if err == nil {
		results = append(results, parseAllConfigsString(string(data))...)
	}

	files, err := os.ReadDir(InterfacesDPath)
	if err == nil {
		for _, f := range files {
			if !f.IsDir() {
				d, err := os.ReadFile(filepath.Join(InterfacesDPath, f.Name()))
				if err == nil {
					results = append(results, parseAllConfigsString(string(d))...)
				}
			}
		}
	}

	// Dedup: later files override earlier ones
	seen := make(map[string]int)
	var dedup []model.NetworkInterface
	for _, iface := range results {
		if idx, ok := seen[iface.Name]; ok {
			dedup[idx] = iface
		} else {
			seen[iface.Name] = len(dedup)
			dedup = append(dedup, iface)
		}
	}
	return dedup
}

func unifiedToWireless(uc model.UnifiedInterfaceConfig, ifaceName string) *model.WirelessConfig {
	if uc.Mode == "" || uc.Mode == "manual" {
		return nil
	}
	w := &model.WirelessConfig{Mode: uc.Mode}
	if uc.Client != nil {
		w.SSID = uc.Client.SSID
		w.Password = uc.Client.Password
	}
	if uc.Hotspot != nil {
		w.ApSsid = uc.Hotspot.SSID
		w.ApPassword = uc.Hotspot.Password
		w.Channel = uc.Hotspot.Channel
	}
	return w
}

// detectInterfaceType checks the actual hardware type via sysfs.
func detectInterfaceType(name string) string {
	// Check by name prefix first (fast path)
	if strings.HasPrefix(name, "wl") || strings.HasPrefix(name, "wlan") {
		return "wifi"
	}
	// Check sysfs uevent for Thunderbolt driver
	ueventPath := fmt.Sprintf("/sys/class/net/%s/device/uevent", name)
	if data, err := os.ReadFile(ueventPath); err == nil {
		if strings.Contains(string(data), "thunderbolt") {
			return "thunderbolt"
		}
	}
	return ""
}

func setHybridCapable(results []model.NetworkInterface) {
	for i := range results {
		if results[i].Type == "wifi" && !results[i].IsVirtual {
			results[i].HybridCapable = checkConcurrentSupport(results[i].Name)
		}
	}
}

// checkConcurrentSupport checks if a WiFi interface supports AP+Station concurrent mode.
// It reads the phy80211 name, then parses `iw phy <name> info` output for
// valid interface combinations that include both AP and managed (station).
func checkConcurrentSupport(ifaceName string) bool {
	// 1. Get phy name from sysfs
	phyPath := fmt.Sprintf("/sys/class/net/%s/phy80211/name", ifaceName)
	phyBytes, err := os.ReadFile(phyPath)
	if err != nil {
		return false
	}
	phyName := strings.TrimSpace(string(phyBytes))
	if phyName == "" {
		return false
	}

	// 2. Run iw phy info
	out, err := exec.Command("iw", "phy", phyName, "info").Output()
	if err != nil {
		return false
	}

	output := string(out)

	// 3. Look for valid interface combinations that contain both AP and managed
	inCombinations := false
	hasAP := false
	hasManaged := false
	braceDepth := 0

	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.Contains(trimmed, "valid interface combinations") {
			inCombinations = true
			hasAP = false
			hasManaged = false
			braceDepth = 0
			continue
		}

		if !inCombinations {
			continue
		}

		// Track brace depth to know when the combinations block ends
		braceDepth += strings.Count(trimmed, "{") - strings.Count(trimmed, "}")

		// Check for AP and managed/station in the same combination
		if strings.Contains(trimmed, "#{ AP }") || strings.Contains(trimmed, "#{ AP,") {
			hasAP = true
		}
		if strings.Contains(trimmed, "#{ managed }") || strings.Contains(trimmed, "#{ managed,") {
			hasManaged = true
		}

		// Also check for combined format: #{ AP, managed }
		if strings.Contains(trimmed, "AP") && strings.Contains(trimmed, "managed") {
			hasAP = true
			hasManaged = true
		}

		// At end of block, check if both modes are supported
		if braceDepth <= 0 && line != "" {
			if hasAP && hasManaged {
				return true
			}
			// Reset for potential next combination
			hasAP = false
			hasManaged = false
		}
	}

	return false
}

// EnsureInterfacesD ensures the main interfaces file includes the source directive
// and the interfaces.d directory exists.
func EnsureInterfacesD() error {
	if err := os.MkdirAll(InterfacesDPath, 0755); err != nil {
		return fmt.Errorf("failed to create interfaces.d directory: %w", err)
	}

	data, err := os.ReadFile(InterfacesPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read %s: %w", InterfacesPath, err)
	}

	content := string(data)
	if !strings.Contains(content, "source /etc/network/interfaces.d/*") {
		// We'll append it safely
		if len(content) > 0 && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += "source /etc/network/interfaces.d/*\n"
		if err := os.WriteFile(InterfacesPath, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to append source directive to %s: %w", InterfacesPath, err)
		}
	}
	return nil
}

// ── Zone Persistence ──────────────────────────────────────────────────────────
// Zone is NimoOS-specific metadata (not part of standard Debian iface format).
// Stored separately in JSON so it survives read/write cycles.

type zoneMap map[string]string // iface name → zone

func loadZones() zoneMap {
	data, err := os.ReadFile(ZoneConfigFile)
	if err != nil {
		return make(zoneMap)
	}
	var zones zoneMap
	if json.Unmarshal(data, &zones) != nil {
		return make(zoneMap)
	}
	return zones
}

func saveZones(zones zoneMap) error {
	if err := os.MkdirAll(NimoosConfDir, 0755); err != nil {
		return fmt.Errorf("failed to create %s: %w", NimoosConfDir, err)
	}
	data, err := json.MarshalIndent(zones, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal zones: %w", err)
	}
	return os.WriteFile(ZoneConfigFile, data, 0644)
}

// ── WiFi Mode Persistence ──────────────────────────────────────────────────────
// Stores the user-set wireless mode (client/ap/concurrent) explicitly.
// The mode is SET by the user, not inferred from data.

type wifiModeMap map[string]string // iface name → mode

func loadWifiModes() wifiModeMap {
	data, err := os.ReadFile(WifiModeConfigFile)
	if err != nil {
		return make(wifiModeMap)
	}
	var modes wifiModeMap
	if json.Unmarshal(data, &modes) != nil {
		return make(wifiModeMap)
	}
	return modes
}

func saveWifiMode(iface, mode string) error {
	if err := os.MkdirAll(NimoosConfDir, 0755); err != nil {
		return fmt.Errorf("failed to create %s: %w", NimoosConfDir, err)
	}
	all := loadWifiModes()
	if mode == "" {
		delete(all, iface)
	} else {
		all[iface] = mode
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal wifi modes: %w", err)
	}
	return os.WriteFile(WifiModeConfigFile, data, 0644)
}

// ── AP Config Persistence ─────────────────────────────────────────────────────
// AP wireless config stored separately from interfaces.d/ to maintain Debian
// compatibility.

type apConfigEntry struct {
	ApSsid   string `json:"apSsid"`
	Password string `json:"password,omitempty"`
	Channel  int    `json:"channel,omitempty"`
}

type apConfigMap map[string]apConfigEntry // iface name → ap config

func loadApConfigs() apConfigMap {
	data, err := os.ReadFile(ApConfigFile)
	if err != nil {
		return make(apConfigMap)
	}
	var cfgs apConfigMap
	if json.Unmarshal(data, &cfgs) != nil {
		return make(apConfigMap)
	}
	return cfgs
}

func saveApConfig(iface string, cfg model.WirelessConfig) error {
	if err := os.MkdirAll(NimoosConfDir, 0755); err != nil {
		return fmt.Errorf("failed to create %s: %w", NimoosConfDir, err)
	}
	all := loadApConfigs()
	pwd := cfg.ApPassword
	if pwd == "" {
		pwd = cfg.Password // fallback for backward compatibility
	}
	all[iface] = apConfigEntry{
		ApSsid:   cfg.ApSsid,
		Password: pwd,
		Channel:  cfg.Channel,
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal AP config: %w", err)
	}
	return os.WriteFile(ApConfigFile, data, 0644)
}

func deleteApConfig(iface string) error {
	all := loadApConfigs()
	delete(all, iface)
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal AP config: %w", err)
	}
	return os.WriteFile(ApConfigFile, data, 0644)
}

// WriteInterfaceConfig writes a single interface configuration to interfaces.d/
// WriteInterfaceConfig saves the interface config to the unified config file (single source of truth),
// then generates derived configs for Debian compatibility (interfaces.d, zones.json, ap.json).
func WriteInterfaceConfig(iface model.NetworkInterface) error {
	// Load existing config to merge (so partial updates don't lose data)
	cfgs := loadUnifiedConfig()
	existing, hasExisting := cfgs[iface.Name]

	// 1. Build unified config entry — start from existing if present
	uc := model.UnifiedInterfaceConfig{
		Mode: iface.Wireless.Mode,
		Zone: iface.Zone,
	}
	if hasExisting {
		uc = existing
		uc.Mode = "manual"
		uc.Zone = iface.Zone
	}
	if iface.Wireless != nil {
		uc.Mode = iface.Wireless.Mode
		// Only overwrite client config when SSID is explicitly provided
		if iface.Wireless.SSID != "" || iface.Wireless.Password != "" {
			uc.Client = &model.ClientConfig{
				SSID:     iface.Wireless.SSID,
				Password: iface.Wireless.Password,
			}
		}
		// Clear client config when mode is not client/concurrent and SSID is empty
		if iface.Wireless.SSID == "" && iface.Wireless.Mode != "client" && iface.Wireless.Mode != "concurrent" {
			uc.Client = nil
		}
		// Clear client config when SSID is explicitly empty in client/concurrent mode (disconnect)
		if iface.Wireless.SSID == "" && (iface.Wireless.Mode == "client" || iface.Wireless.Mode == "concurrent") {
			uc.Client = nil
		}
		// Only overwrite hotspot config when ApSsid is explicitly provided
		if iface.Wireless.ApSsid != "" {
			password := iface.Wireless.ApPassword
			if password == "" {
				password = iface.Wireless.Password
			}
			uc.Hotspot = &model.HotspotConfig{
				SSID:     iface.Wireless.ApSsid,
				Password: password,
				Channel:  iface.Wireless.Channel,
			}
		}
		// Clear hotspot when mode changed away from ap/concurrent and ApSsid is empty
		if iface.Wireless.ApSsid == "" && iface.Wireless.Mode != "ap" && iface.Wireless.Mode != "concurrent" {
			uc.Hotspot = nil
		}
	}
	// Only overwrite IPv4 config when explicitly provided
	if iface.IPv4 != nil {
		uc.IPv4 = iface.IPv4
	} else if iface.Wireless != nil && iface.Wireless.Mode == "client" && uc.IPv4 != nil && uc.IPv4.Method == "static" {
		// Switching from AP/client mode without explicit IPv4 config.
		// AP mode forces static IP (192.168.22.1), which is not appropriate
		// for client mode — reset to DHCP.
		uc.IPv4 = &model.IPv4Config{Method: "dhcp"}
	}

	// 2. Save to unified config
	cfgs[iface.Name] = uc
	if err := saveUnifiedConfig(cfgs); err != nil {
		return err
	}

	// 3. Generate derived interfaces.d/ file for Debian compatibility
	if err := EnsureInterfacesD(); err != nil {
		return err
	}
	confPath := filepath.Join(InterfacesDPath, fmt.Sprintf("%s.conf", iface.Name))

	method := "manual"
	if iface.IPv4 != nil && iface.IPv4.Method != "" {
		method = iface.IPv4.Method
	}
	if iface.Type == "thunderbolt" {
		method = "static"
	}

	var out bytes.Buffer
	out.WriteString(fmt.Sprintf("auto %s\n", iface.Name))
	out.WriteString(fmt.Sprintf("iface %s inet %s\n", iface.Name, method))

	if iface.Type == "bridge" || iface.IsVirtual {
		out.WriteString(fmt.Sprintf("    bridge_ports %s\n", strings.Join(iface.Ports, " ")))
		out.WriteString("    bridge_stp off\n")
		out.WriteString("    bridge_fd 0\n")
	}

	if iface.IPv4 != nil && method == "static" {
		if iface.IPv4.Address != "" {
			out.WriteString(fmt.Sprintf("    address %s\n", iface.IPv4.Address))
		}
		if iface.IPv4.Netmask != "" {
			out.WriteString(fmt.Sprintf("    netmask %s\n", iface.IPv4.Netmask))
		}
		if iface.IPv4.Gateway != "" {
			out.WriteString(fmt.Sprintf("    gateway %s\n", iface.IPv4.Gateway))
		}
		if len(iface.IPv4.DNS) > 0 {
			out.WriteString(fmt.Sprintf("    dns-nameservers %s\n", strings.Join(iface.IPv4.DNS, " ")))
		}
	}

	if iface.Wireless != nil && (iface.Wireless.Mode == "client" || iface.Wireless.Mode == "concurrent") {
		if iface.Wireless.SSID != "" {
			out.WriteString(fmt.Sprintf("    wpa-ssid %s\n", iface.Wireless.SSID))
		}
		if iface.Wireless.Password != "" {
			out.WriteString(fmt.Sprintf("    wpa-psk %s\n", iface.Wireless.Password))
		}
	}
	out.WriteString("\n")

	if err := os.WriteFile(confPath, out.Bytes(), 0644); err != nil {
		return err
	}

	// 4. Generate derived zone config
	zones := loadZones()
	if iface.Zone != "" {
		zones[iface.Name] = iface.Zone
	} else {
		delete(zones, iface.Name)
	}
	if err := saveZones(zones); err != nil {
		return err
	}

	// 5. Generate derived AP config
	if iface.Wireless != nil && (iface.Wireless.Mode == "ap" || iface.Wireless.Mode == "concurrent") {
		return saveApConfig(iface.Name, *iface.Wireless)
	}
	return deleteApConfig(iface.Name)
}

// ApplyInterfaceIP applies the IP configuration to a network interface using ip commands.
// This is needed for AP mode where the interface needs a static IP immediately.
func ApplyInterfaceIP(name string, ipv4 *model.IPv4Config) error {
	if ipv4 == nil {
		return nil
	}
	// Always bring the interface up
	_ = exec.Command("ip", "link", "set", name, "up").Run()

	if ipv4.Method == "dhcp" || ipv4.Method == "manual" {
		// Flush any stale static IP (e.g. from previous AP mode)
		_ = exec.Command("ip", "addr", "flush", "dev", name).Run()
		// Run dhclient to obtain IP via DHCP
		_ = exec.Command("dhclient", "-v", name).Run()
		// Also try udhcpc as fallback
		_ = exec.Command("udhcpc", "-i", name, "-n", "-q").Run()
		return nil
	}
	// Static: assign the configured IP
	addr := ipv4.Address
	netmask := ipv4.Netmask
	if addr == "" {
		// Default IP for AP mode
		addr = "192.168.22.1"
		netmask = "24"
	}
	if netmask == "" || netmask == "255.255.255.0" {
		netmask = "24"
	} else if netmask == "255.255.0.0" {
		netmask = "16"
	} else if netmask == "255.0.0.0" {
		netmask = "8"
	}
	// Flush existing IPs and assign new one
	_ = exec.Command("ip", "addr", "flush", "dev", name).Run()
	_ = exec.Command("ip", "link", "set", name, "up").Run()
	return exec.Command("ip", "addr", "add", fmt.Sprintf("%s/%s", addr, netmask), "dev", name).Run()
}

// ReadInterfaceConfig reads a single interface configuration from interfaces.d/ or the main file
func ReadInterfaceConfig(name string) (*model.NetworkInterface, error) {
	all, err := GetAllInterfaceConfigs()
	if err != nil {
		return nil, err
	}
	for _, iface := range all {
		if iface.Name == name {
			// Return a copy
			cp := iface
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("interface %s not found", name)
}

// GetAllInterfaceConfigs reads all interfaces from the unified config (single source of truth).
// Derived configs (interfaces.d, ap.json, zones.json) are outputs generated on save, not inputs.
// GetInterfaceConfig returns the persisted config for a single interface.
func GetInterfaceConfig(name string) (*model.NetworkInterface, error) {
	all, err := GetAllInterfaceConfigs()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, nil
}

func GetAllInterfaceConfigs() ([]model.NetworkInterface, error) {
	unified := loadUnifiedConfig()

	var results []model.NetworkInterface
	for name, uc := range unified {
		t := detectInterfaceType(name)
		if t == "" {
			t = "ethernet"
		}
		iface := model.NetworkInterface{
			Name:          name,
			Type:          t,
			IsVirtual:     isVirtualIface(name),
			IPv4:          uc.IPv4,
			Zone:          uc.Zone,
			Wireless:      unifiedToWireless(uc, name),
			HybridCapable: false,
		}
		results = append(results, iface)
	}

	// 5. Set HybridCapable for WiFi interfaces
	setHybridCapable(results)

	return results, nil
}

func isVirtualIface(name string) bool {
	return strings.HasPrefix(name, "zt") || name == "docker0" || strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "veth") || name == "lo" || strings.HasPrefix(name, VirtualApIfacePrefix)
}

func parseAllConfigsString(content string) []model.NetworkInterface {
	var results []model.NetworkInterface
	scanner := bufio.NewScanner(strings.NewReader(content))

	var current *model.NetworkInterface

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		m := ifaceLineRe.FindStringSubmatch(trimmed)
		if m != nil {
			if current != nil {
				results = append(results, *current)
			}
			name := m[1]
			current = &model.NetworkInterface{
				Name: name,
				Type: detectInterfaceType(name),
				IPv4: &model.IPv4Config{Method: m[2]},
			}
			continue
		}

		if current != nil {
			if trimmed == "" || strings.HasPrefix(trimmed, "auto ") || strings.HasPrefix(trimmed, "source ") {
				results = append(results, *current)
				current = nil
				continue
			}
			if strings.HasPrefix(trimmed, "iface ") {
				// new iface block but didn't match regex (e.g. inet6)
				results = append(results, *current)
				current = nil
				continue
			}

			if strings.HasPrefix(trimmed, "address ") {
				current.IPv4.Address = strings.TrimSpace(strings.TrimPrefix(trimmed, "address "))
			} else if strings.HasPrefix(trimmed, "netmask ") {
				current.IPv4.Netmask = strings.TrimSpace(strings.TrimPrefix(trimmed, "netmask "))
			} else if strings.HasPrefix(trimmed, "gateway ") {
				current.IPv4.Gateway = strings.TrimSpace(strings.TrimPrefix(trimmed, "gateway "))
			} else if strings.HasPrefix(trimmed, "dns-nameservers ") {
				current.IPv4.DNS = strings.Fields(strings.TrimPrefix(trimmed, "dns-nameservers "))
			} else if strings.HasPrefix(trimmed, "bridge_ports ") {
				current.IsVirtual = true
				current.Type = "bridge"
				current.Ports = strings.Fields(strings.TrimPrefix(trimmed, "bridge_ports "))
			} else if strings.HasPrefix(trimmed, "wpa-ssid ") {
				if current.Wireless == nil {
					current.Wireless = &model.WirelessConfig{Mode: "client"}
				}
				if current.Type == "" {
					current.Type = "wifi"
				}
				current.Wireless.SSID = strings.TrimSpace(strings.TrimPrefix(trimmed, "wpa-ssid "))
			} else if strings.HasPrefix(trimmed, "wpa-psk ") {
				if current.Wireless == nil {
					current.Wireless = &model.WirelessConfig{Mode: "client"}
				}
				if current.Type == "" {
					current.Type = "wifi"
				}
				current.Wireless.Password = strings.TrimSpace(strings.TrimPrefix(trimmed, "wpa-psk "))
			}
		}
	}

	if current != nil {
		results = append(results, *current)
	}

	// Set default type for interfaces that were not explicitly typed
	for i := range results {
		if results[i].Type == "" {
			if t := detectInterfaceType(results[i].Name); t != "" {
				results[i].Type = t
			} else {
				results[i].Type = "ethernet"
			}
		}
	}

	return results
}

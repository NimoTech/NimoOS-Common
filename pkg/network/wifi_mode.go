package network

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/NimoTech/NimoOS-Common/model"
	"github.com/NimoTech/NimoOS-Common/utils/logger"
	"go.uber.org/zap"
)

const (
	VirtualApIfacePrefix = "wlan_ap"
	wpaSupplicantConfDir = "/etc/wpa_supplicant"
)

// ApplyWirelessMode switches the wireless interface to the target mode at runtime.
// Must be called AFTER WriteInterfaceConfig (config saved) but BEFORE ApplyApConfig.
func ApplyWirelessMode(ifaceName string, wireless *model.WirelessConfig) error {
	if wireless == nil || wireless.Mode == "" {
		return nil
	}

	switch wireless.Mode {
	case "client":
		return setClientMode(ifaceName, wireless.SSID, wireless.Password)
	case "ap":
		return setAPMode(ifaceName)
	case "concurrent":
		return setConcurrentMode(ifaceName, wireless.SSID, wireless.Password, wireless.ApSsid, wireless.ApPassword, wireless.Channel)
	default:
		return fmt.Errorf("unknown wireless mode: %s", wireless.Mode)
	}
}

// ── Client Mode ──────────────────────────────────────────────────────────────

func setClientMode(ifaceName, ssid, password string) error {
	logger.Info("Switching to client mode", zap.String("iface", ifaceName))

	// 1. Stop hostapd (if running)
	_ = exec.Command("systemctl", "stop", "hostapd").Run()

	// 2. Clean up any stale virtual AP interfaces
	cleanupVirtualIfaces(ifaceName)

	// 3. Kill any existing dhcpcd on this interface (from previous run)
	_ = exec.Command("dhcpcd", "-k", ifaceName).Run()
	_ = exec.Command("dhcpcd", "--release", ifaceName).Run()

	// 4. Switch the physical interface to managed (station) type
	out, err := exec.Command("iw", "dev", ifaceName, "set", "type", "managed").CombinedOutput()
	if err != nil {
		logger.Info("iw set type managed failed (may already be managed)", zap.String("output", string(out)))
	}
	_ = exec.Command("ip", "link", "set", ifaceName, "up").Run()

	// 5. Generate wpa_supplicant.conf and restart
	if ssid != "" {
		if err := generateAndStartWpaSupplicant(ifaceName, ssid, password); err != nil {
			return fmt.Errorf("failed to configure wpa_supplicant: %w", err)
		}
	}

	// 6. Run dhcpcd in background to get IP once wpa_supplicant connects.
	_ = exec.Command("dhcpcd", ifaceName).Start()

	return nil
}

// ── AP Mode ──────────────────────────────────────────────────────────────────

func setAPMode(ifaceName string) error {
	logger.Info("Switching to AP mode", zap.String("iface", ifaceName))

	// 1. Stop interface-specific wpa_supplicant (keep global one running)
	stopWpaSupplicantForIface(ifaceName)

	// 2. Kill dhcpcd on this interface (it holds IP and may block mode switch)
	_ = exec.Command("dhcpcd", "-k", ifaceName).Run()
	_ = exec.Command("dhcpcd", "--release", ifaceName).Run()

	// 3. Clean up any stale virtual AP interfaces
	cleanupVirtualIfaces(ifaceName)

	// 4. Switch to AP type — note: this requires hostapd to be running to stick
	out, err := exec.Command("iw", "dev", ifaceName, "set", "type", "ap").CombinedOutput()
	if err != nil {
		// This may fail with "operation not supported" on some drivers — that's OK,
		// hostapd will set the type when it starts.
		logger.Info("iw set type ap (may need hostapd)", zap.String("output", string(out)))
	}
	_ = exec.Command("ip", "link", "set", ifaceName, "up").Run()

	// hostapd will be restarted by ApplyApConfig (called separately)
	return nil
}

// ── Concurrent Mode ──────────────────────────────────────────────────────────

func setConcurrentMode(ifaceName, ssid, password, apSsid, apPassword string, channel int) error {
	logger.Info("Switching to concurrent mode", zap.String("iface", ifaceName))

	// 1. Stop services
	_ = exec.Command("systemctl", "stop", "hostapd").Run()
	stopWpaSupplicantForIface(ifaceName)

	// 2. Kill dhcpcd on this interface (it holds IP and may block mode switch)
	_ = exec.Command("dhcpcd", "-k", ifaceName).Run()
	_ = exec.Command("dhcpcd", "--release", ifaceName).Run()

	// 3. Switch main interface to managed
	out, err := exec.Command("iw", "dev", ifaceName, "set", "type", "managed").CombinedOutput()
	if err != nil {
		logger.Info("iw set type managed", zap.String("output", string(out)))
	}
	_ = exec.Command("ip", "link", "set", ifaceName, "up").Run()

	// 3. Clean up stale virtual interfaces and create new virtual AP interface
	cleanupVirtualIfaces(ifaceName)

	apIface := virtualApName(ifaceName)
	if err := createVirtualAP(ifaceName, apIface); err != nil {
		return fmt.Errorf("failed to create virtual AP interface: %w", err)
	}

	// 4. Configure wpa_supplicant for client connection on main interface
	if ssid != "" {
		if err := generateAndStartWpaSupplicant(ifaceName, ssid, password); err != nil {
			return fmt.Errorf("failed to configure wpa_supplicant for concurrent: %w", err)
		}
	}

	// Hostapd conf and restart will be handled by ApplyApConfig (called by the route handler).
	// Do NOT write hostapd.conf or restart hostapd here.

	return nil
}

// ── Virtual Interface Helpers ───────────────────────────────────────────────

func virtualApName(phyIface string) string {
	return VirtualApIfacePrefix
}

func cleanupVirtualIfaces(phyIface string) {
	// List all wireless interfaces and remove any that look like virtual APs
	out, err := exec.Command("iw", "dev").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Interface") {
			parts := strings.Split(line, " ")
			if len(parts) >= 2 {
				name := parts[len(parts)-1]
				if strings.HasPrefix(name, VirtualApIfacePrefix) {
					_ = exec.Command("iw", "dev", name, "del").Run()
				}
			}
		}
	}
}

func createVirtualAP(phyIface, apIface string) error {
	out, err := exec.Command("iw", "dev", phyIface, "interface", "add", apIface, "type", "__ap").CombinedOutput()
	if err != nil {
		return fmt.Errorf("iw interface add failed: %s: %w", string(out), err)
	}
	return nil
}

// ── wpa_supplicant ──────────────────────────────────────────────────────────

func generateAndStartWpaSupplicant(ifaceName, ssid, password string) error {
	if err := os.MkdirAll(wpaSupplicantConfDir, 0755); err != nil {
		return err
	}

	confPath := fmt.Sprintf("%s/%s.conf", wpaSupplicantConfDir, ifaceName)

	// Use wpa_passphrase to generate the PSK-encrypted config
	var conf string
	if password != "" {
		// Try wpa_passphrase for proper PSK generation
		cmd := exec.Command("wpa_passphrase", ssid, password)
		out, err := cmd.Output()
		if err == nil {
			conf = string(out)
		} else {
			// Fallback: plain text psk (some embedded drivers)
			conf = fmt.Sprintf(`network={
	ssid="%s"
	psk="%s"
	key_mgmt=WPA-PSK
}
`, ssid, password)
		}
	} else {
		// Open network
		conf = fmt.Sprintf(`network={
	ssid="%s"
	key_mgmt=NONE
}
`, ssid)
	}

	// Prepend the control interface configuration
	conf = fmt.Sprintf("ctrl_interface=DIR=/run/wpa_supplicant\nupdate_config=1\n\n%s", conf)

	if err := os.WriteFile(confPath, []byte(conf), 0600); err != nil {
		return err
	}

	// Kill existing wpa_supplicant for this interface
	stopWpaSupplicantForIface(ifaceName)

	// Start new wpa_supplicant with the config
	pidFile := fmt.Sprintf("/run/wpa_supplicant.%s.pid", ifaceName)
	cmd := exec.Command("wpa_supplicant", "-s", "-B",
		"-P", pidFile,
		"-i", ifaceName,
		"-D", "nl80211,wext",
		"-c", confPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("wpa_supplicant start failed: %s: %w", string(out), err)
	}

	return nil
}

func stopWpaSupplicantForIface(ifaceName string) {
	pidFile := fmt.Sprintf("/run/wpa_supplicant.%s.pid", ifaceName)
	if data, err := os.ReadFile(pidFile); err == nil {
		pid := strings.TrimSpace(string(data))
		if pid != "" {
			_ = exec.Command("kill", pid).Run()
		}
	}
	// Also try via wpa_cli
	_ = exec.Command("wpa_cli", "-i", ifaceName, "terminate").Run()
}

// ── hostapd conf generation (for concurrent mode) ──────────────────────────

func generateHostapdConf(apIface, ssid, password string, channel int) string {
	hwMode := "g"
	if channel > 14 {
		hwMode = "a"
	}
	if channel <= 0 {
		channel = 6
	}

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("interface=%s\n", apIface))
	buf.WriteString("driver=nl80211\n")
	buf.WriteString(fmt.Sprintf("ssid=%s\n", ssid))
	buf.WriteString(fmt.Sprintf("hw_mode=%s\n", hwMode))
	buf.WriteString(fmt.Sprintf("channel=%d\n", channel))
	buf.WriteString("wmm_enabled=1\n")
	buf.WriteString("macaddr_acl=0\n")
	buf.WriteString("auth_algs=1\n")
	buf.WriteString("ignore_broadcast_ssid=0\n")
	if password != "" {
		buf.WriteString("wpa=2\n")
		buf.WriteString(fmt.Sprintf("wpa_passphrase=%s\n", password))
		buf.WriteString("wpa_key_mgmt=WPA-PSK\n")
		buf.WriteString("wpa_pairwise=TKIP\n")
		buf.WriteString("rsn_pairwise=CCMP\n")
	}
	return buf.String()
}

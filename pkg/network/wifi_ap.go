package network

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/NimoTech/NimoOS-Common/model"
	"github.com/NimoTech/NimoOS-Common/utils/logger"
	"go.uber.org/zap"
)

const hostapdConfPath = "/etc/hostapd/hostapd.conf"

var watchdogOnce sync.Once

var hostapdTmpl = `
interface={{.Interface}}
driver=nl80211
ssid={{.SSID}}
hw_mode={{if gt .Channel 14}}a{{else}}g{{end}}
channel={{.Channel}}
wmm_enabled=1
macaddr_acl=0
auth_algs=1
ignore_broadcast_ssid=0
{{if .Password}}
wpa=2
wpa_passphrase={{.Password}}
wpa_key_mgmt=WPA-PSK
wpa_pairwise=TKIP
rsn_pairwise=CCMP
{{end}}
`

// ApplyApConfig generates the hostapd.conf and restarts the hostapd service.
// For concurrent mode, it uses the virtual AP interface (wlan_ap) instead of the physical one.
func ApplyApConfig(iface string, config model.WirelessConfig) error {
	tmpl, err := template.New("hostapd").Parse(hostapdTmpl)
	if err != nil {
		return err
	}

	// In concurrent mode, hostapd runs on the virtual AP interface
	hostapdIface := iface
	if config.Mode == "concurrent" {
		hostapdIface = VirtualApIfacePrefix
	}

	ssid := config.ApSsid
	if ssid == "" {
		ssid = config.SSID
	}
	pwd := config.ApPassword
	if pwd == "" {
		pwd = config.Password
	}

	data := struct {
		Interface string
		SSID      string
		Password  string
		Channel   int
	}{
		Interface: hostapdIface,
		SSID:      ssid,
		Password:  pwd,
		Channel:   config.Channel,
	}

	if data.Channel == 0 {
		// Default to channel 6 if not specified
		data.Channel = 6
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return err
	}

	if err := os.WriteFile(hostapdConfPath, buf.Bytes(), 0644); err != nil {
		return err
	}

	// For concurrent mode, set up the virtual AP interface with IP
	if config.Mode == "concurrent" {
		_ = exec.Command("ip", "addr", "add", "192.168.22.1/24", "dev", hostapdIface).Run()
		_ = exec.Command("ip", "link", "set", hostapdIface, "up").Run()
	}

	// Restart hostapd
	cmd := exec.Command("systemctl", "restart", "hostapd")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to restart hostapd: %w", err)
	}

	// Start channel watchdog for concurrent mode
	if config.Mode == "concurrent" {
		StartWifiWatchdog(iface, VirtualApIfacePrefix)
	}

	return nil
}

// StartWifiWatchdog starts a background goroutine to monitor upstream channel changes
// and auto-sync the AP channel if running in Concurrent AP+STA mode.
// Uses sync.Once to prevent duplicate goroutines.
func StartWifiWatchdog(clientIface, apIface string) {
	watchdogOnce.Do(func() {
		go runWatchdog(clientIface, apIface)
	})
}

func runWatchdog(clientIface, apIface string) {
	logger.Info("Starting Wi-Fi Channel Watchdog", zap.String("client", clientIface), zap.String("ap", apIface))
	var lastChannel int

	for {
		time.Sleep(10 * time.Second)

		// 1. Get current client channel
		cmd := exec.Command("iw", "dev", clientIface, "link")
		out, err := cmd.Output()
		if err != nil {
			continue
		}

		channel := parseChannelFromIwLink(string(out))
		if channel == 0 || channel == lastChannel {
			continue
		}

		lastChannel = channel
		logger.Info("Upstream channel changed, syncing AP...", zap.Int("new_channel", channel))

		// 2. Rewrite hostapd.conf with new channel + hw_mode
		content, err := os.ReadFile(hostapdConfPath)
		if err != nil {
			continue
		}
		newContent := updateChannelInHostapd(string(content), channel)
		_ = os.WriteFile(hostapdConfPath, []byte(newContent), 0644)

		// 3. Hot restart AP
		_ = exec.Command("systemctl", "restart", "hostapd").Run()
	}
}

func parseChannelFromIwLink(output string) int {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "freq: ") {
			var freq int
			fmt.Sscanf(strings.TrimSpace(line), "freq: %d", &freq)
			// 2.4GHz: 2412-2484
			if freq >= 2412 && freq <= 2484 {
				return (freq - 2407) / 5
			}
			// 5GHz: 5170-5825
			if freq >= 5170 && freq <= 5825 {
				return (freq - 5000) / 5
			}
		}
	}
	return 0
}

func updateChannelInHostapd(content string, channel int) string {
	lines := strings.Split(content, "\n")
	hasHwMode := false
	for i, line := range lines {
		if strings.HasPrefix(line, "channel=") {
			lines[i] = fmt.Sprintf("channel=%d", channel)
		}
		if strings.HasPrefix(line, "hw_mode=") {
			hasHwMode = true
			if channel > 14 {
				lines[i] = "hw_mode=a"
			} else {
				lines[i] = "hw_mode=g"
			}
		}
	}
	// Insert hw_mode if missing
	if !hasHwMode {
		hwMode := "g"
		if channel > 14 {
			hwMode = "a"
		}
		lines = append([]string{fmt.Sprintf("hw_mode=%s", hwMode)}, lines...)
	}
	return strings.Join(lines, "\n")
}

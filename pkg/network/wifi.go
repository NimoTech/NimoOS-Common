package network

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var validIfaceRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,15}$`)

type WifiNetwork struct {
	SSID      string `json:"ssid"`
	BSSID     string `json:"bssid"`
	Signal    int    `json:"signal"`
	Channel   int    `json:"channel"`
	Secure    bool   `json:"secure"`
	Connected bool   `json:"connected"`
}

// ScanWifi uses `iw` to scan for wireless networks on a given interface.
// Also queries the currently connected network via `iw dev <iface> link` and
// ensures it is present in the returned list.
func ScanWifi(iface string) ([]WifiNetwork, error) {
	if !validIfaceRe.MatchString(iface) {
		return nil, fmt.Errorf("invalid interface name: %q", iface)
	}
	// e.g. iw dev wlp1950s scan
	cmd := exec.Command("iw", "dev", iface, "scan")
	out, err := cmd.Output()
	if err != nil {
		// scan may fail in AP mode (operation not supported); return empty list
		return nil, nil
	}

	results := parseIwScan(string(out))

	// Also get the currently connected network and ensure it's in the list
	if connected := getConnectedNetwork(iface); connected != nil {
		connected.Connected = true
		found := false
		for i, n := range results {
			if n.SSID == connected.SSID {
				results[i].Connected = true
				found = true
				break
			}
		}
		if !found {
			results = append(results, *connected)
		}
	}

	return results, nil
}

// getConnectedNetwork queries the currently connected AP via `iw dev <iface> link`.
func getConnectedNetwork(iface string) *WifiNetwork {
	cmd := exec.Command("iw", "dev", iface, "link")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseIwLink(string(out))
}

// parseIwLink parses the output of `iw dev <iface> link`.
// Example:
//
//	Connected to 00:11:22:33:44:55 (on wlp195s0)
//		SSID: MyNetwork
//		freq: 2437
//		signal: -45 dBm
//		RX: ...
func parseIwLink(output string) *WifiNetwork {
	net := &WifiNetwork{}
	ssidRe := regexp.MustCompile(`SSID:\s*(.*)$`)
	signalRe := regexp.MustCompile(`signal:\s*([-0-9.]+)\s*dBm`)
	bssRe := regexp.MustCompile(`Connected to\s+([0-9a-fA-F:]+)`)

	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if m := bssRe.FindStringSubmatch(trimmed); m != nil {
			net.BSSID = m[1]
		} else if m := ssidRe.FindStringSubmatch(trimmed); m != nil {
			net.SSID = m[1]
		} else if m := signalRe.FindStringSubmatch(trimmed); m != nil {
			sig, _ := strconv.ParseFloat(m[1], 64)
			net.Signal = int(sig)
		}
	}
	if net.SSID == "" {
		return nil
	}
	return net
}

// parseIwScan parses the output of `iw dev <iface> scan`
func parseIwScan(output string) []WifiNetwork {
	var networks []WifiNetwork
	var current *WifiNetwork

	lines := strings.Split(output, "\n")

	// Example format:
	// BSS 00:11:22:33:44:55(on wlp195s0)
	//         SSID: MyNetwork
	//         signal: -45.00 dBm
	//         DS Parameter set: channel 6
	//         RSN:     * Version: 1

	bssRe := regexp.MustCompile(`^BSS\s+([0-9a-fA-F:]+)`)
	ssidRe := regexp.MustCompile(`SSID:\s*(.*)$`)
	signalRe := regexp.MustCompile(`signal:\s*([-0-9.]+)\s*dBm`)
	channelRe := regexp.MustCompile(`DS Parameter set: channel\s*([0-9]+)`)
	primaryChRe := regexp.MustCompile(`\* primary channel:\s*([0-9]+)`) // Used in 5GHz sometimes

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if m := bssRe.FindStringSubmatch(line); m != nil {
			if current != nil && current.SSID != "" {
				networks = append(networks, *current)
			}
			current = &WifiNetwork{
				BSSID: m[1],
			}
			continue
		}

		if current != nil {
			if m := ssidRe.FindStringSubmatch(trimmed); m != nil {
				current.SSID = m[1]
			} else if m := signalRe.FindStringSubmatch(trimmed); m != nil {
				sig, _ := strconv.ParseFloat(m[1], 64)
				current.Signal = int(sig)
			} else if m := channelRe.FindStringSubmatch(trimmed); m != nil {
				ch, _ := strconv.Atoi(m[1])
				current.Channel = ch
			} else if m := primaryChRe.FindStringSubmatch(trimmed); m != nil {
				ch, _ := strconv.Atoi(m[1])
				if current.Channel == 0 {
					current.Channel = ch
				}
			} else if strings.HasPrefix(trimmed, "RSN:") || strings.HasPrefix(trimmed, "WPA:") {
				current.Secure = true
			}
		}
	}

	if current != nil && current.SSID != "" {
		networks = append(networks, *current)
	}

	// Filter out duplicates (keep strongest signal)
	return deduplicateWifi(networks)
}

func deduplicateWifi(networks []WifiNetwork) []WifiNetwork {
	seen := make(map[string]WifiNetwork)
	for _, n := range networks {
		if existing, ok := seen[n.SSID]; ok {
			if n.Signal > existing.Signal {
				seen[n.SSID] = n
			}
		} else {
			seen[n.SSID] = n
		}
	}

	var results []WifiNetwork
	for _, v := range seen {
		// Ignore hidden SSIDs which often show up as empty or \x00
		if strings.TrimSpace(v.SSID) != "" && !strings.Contains(v.SSID, "\\x00") {
			results = append(results, v)
		}
	}
	return results
}

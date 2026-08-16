package network

import (
	"net"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func isPublicIP(ip net.IP) bool {
	//IPv4
	if ip4 := ip.To4(); ip4 != nil {
		privateBlocks := []string{
			"10.0.0.0/8",
			"172.16.0.0/12",
			"192.168.0.0/16",
			"169.254.0.0/16",
			"127.0.0.0/8",
		}

		for _, block := range privateBlocks {
			_, cidr, _ := net.ParseCIDR(block)
			if cidr.Contains(ip) {
				return false
			}
		}
		return true
	}

	//IPv6
	if ip.IsLoopback() || ip.IsUnspecified() {
		return false
	}

	privateBlocks6 := []string{
		"fc00::/7",  // Unique Local Addresses (ULA)
		"fe80::/10", // Link-Local Addresses
	}
	for _, block := range privateBlocks6 {
		_, cidr, _ := net.ParseCIDR(block)
		if cidr.Contains(ip) {
			return false
		}
	}
	return true
}

func GetPublicIPs() ([]string, []string, error) {
	var ipv4List, ipv6List []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, nil, phelixerr.Wrap(phelixerr.CodeNetwork, "failed to enumerate network interfaces", err)
	}

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok {
			ip := ipnet.IP
			if isPublicIP(ip) {
				if ip.To4() != nil {
					ipv4List = append(ipv4List, ip.String())
				} else {
					ipv6List = append(ipv6List, ip.String())
				}
			}
		}
	}
	return ipv4List, ipv6List, nil
}

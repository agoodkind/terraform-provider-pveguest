package netconfig

import (
	_ "embed"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
)

//go:embed scripts/ifupdown_bond.pl
var ifupdownBondScript string

func (s Settings) bondPath() string {
	return "/etc/network/pveguest-bonds/" + hex.EncodeToString([]byte(s.Name))
}

func (s Settings) dhclientConfigPath() string {
	return "/etc/dhcp/pveguest-" + hex.EncodeToString([]byte(s.Name)) + ".conf"
}

func (s Settings) ifupdownBondFiles(
	changes map[string]Edit,
	files map[string]string,
	removing bool,
) error {
	config := "include \"/etc/dhcp/dhclient.conf\";\nsend dhcp-client-identifier 01:" + s.MAC + ";\n"
	for _, file := range []struct {
		name, body string
		absent     bool
	}{
		{s.bondPath() + ".pl", ifupdownBondScript, removing || s.Bond == nil},
		{s.dhclientConfigPath(), config, removing || s.Bond == nil || !s.DHCP4},
	} {
		if err := ownedNetworkFile(changes, files, file.name, file.body, file.absent); err != nil {
			return err
		}
	}

	prefix := "/etc/systemd/network/05-pveguest-member-" + hex.EncodeToString([]byte(s.Name)) + "-"
	links := map[string]string{}
	if s.Bond != nil && !removing {
		for _, member := range s.Bond.Members {
			filename := prefix + hex.EncodeToString([]byte(member.Name)) + ".link"
			links[filename] = fmt.Sprintf(
				"[Match]\nMACAddress=%s\nDriver=!bonding\n\n[Link]\nName=%s\n",
				member.MAC,
				member.Name,
			)
		}
	}

	for filename := range files {
		if strings.HasPrefix(filename, prefix) && path.Ext(filename) == ".link" {
			if _, present := links[filename]; !present {
				links[filename] = ""
			}
		}
	}

	for filename, body := range links {
		if err := ownedNetworkFile(changes, files, filename, body, body == ""); err != nil {
			return err
		}
	}

	return nil
}

func (s Settings) ifupdownBondSetup(out *strings.Builder) {
	fmt.Fprintf(
		out,
		"    pre-up perl %s.pl %s %s %d %s",
		s.bondPath(),
		s.Name,
		s.Bond.Primary,
		s.Bond.MIIMon,
		s.Bond.PrimaryReselect,
	)

	for _, member := range s.Bond.Members {
		fmt.Fprintf(out, " %s %s", member.Name, member.MAC)
	}

	out.WriteByte('\n')
}

func (s Settings) ifupdownBondDHCP(out *strings.Builder) {
	args := fmt.Sprintf(
		"-4 -cf %s -pf /run/dhclient.%s.pid -lf /var/lib/dhcp/dhclient.%s.leases %s",
		s.dhclientConfigPath(),
		s.Name,
		s.Name,
		s.Name,
	)
	fmt.Fprintf(out, "    up dhclient %s\n    down dhclient -r %s\n", args, args)
}

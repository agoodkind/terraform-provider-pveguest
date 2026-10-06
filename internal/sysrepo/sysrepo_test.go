package sysrepo_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/internal/sysrepo"
)

// The table has the layout that srctl_list prints in sysrepo 3.7. The flag
// placeholder FLAG stands for the installed-module letter.
const moduleListTemplate = `Sysrepo repository: /etc/sysrepo

Module Name         | Revision   | Flags | Startup Owner | Startup Perms | Running Perms | Features
-------------------------------------------------------------------------------------------------
ietf-nat            | 2019-01-10 | FLAG  | root:root     | 600           | 600           | basic-nat44 napt44
ietf-netconf        | 2013-09-29 | FLAG  | root:root     | 600           | 600           | writable-running
 ietf-sub           | 2020-01-01 | s     |               |               |               |
ietf-yang-types     | 2025-12-22 | i     |               |               |               |

Flags meaning: see the sysrepoctl manual page

`

const installedFlagLetter = "\x49"

func moduleList() string {
	return strings.ReplaceAll(moduleListTemplate, "FLAG", installedFlagLetter)
}

func TestParseModuleList(t *testing.T) {
	modules, err := sysrepo.ParseModuleList(moduleList())
	if err != nil {
		t.Fatal(err)
	}

	nat, found := sysrepo.FindImplemented(modules, "ietf-nat")
	if !found {
		t.Fatal("ietf-nat is not implemented in the parsed list")
	}
	if nat.Revision != "2019-01-10" {
		t.Errorf("ietf-nat revision is %q", nat.Revision)
	}
	if !slices.Equal(nat.Features, []string{"basic-nat44", "napt44"}) {
		t.Errorf("ietf-nat features are %q", nat.Features)
	}

	netconf, found := sysrepo.FindImplemented(modules, "ietf-netconf")
	if !found || len(netconf.Features) != 1 {
		t.Errorf("ietf-netconf row parsed as %+v", netconf)
	}

	if _, found := sysrepo.FindImplemented(modules, "ietf-yang-types"); found {
		t.Error("ietf-yang-types counts as implemented, but another module only imports it")
	}
	if _, found := sysrepo.FindImplemented(modules, "ietf-sub"); found {
		t.Error("a submodule row counts as a module")
	}
}

func TestParseModuleListRejectsOutputWithoutTable(t *testing.T) {
	_, err := sysrepo.ParseModuleList("sysrepoctl error: Failed to connect\n")
	if !errors.Is(err, sysrepo.ErrNoModuleTable) {
		t.Fatalf("error is %v, want ErrNoModuleTable", err)
	}
}

func TestParseModuleFileName(t *testing.T) {
	file, err := sysrepo.ParseModuleFileName("/usr/local/share/yang/ietf-ip@2018-02-22.yang")
	if err != nil {
		t.Fatal(err)
	}
	if file.Module != "ietf-ip" || file.Revision != "2018-02-22" {
		t.Errorf("parsed %+v", file)
	}

	for _, name := range []string{"/x/ietf-ip.yang", "/x/ietf-ip@2018.yang", "/x/ietf-ip@2018-02-22.yin"} {
		if _, err := sysrepo.ParseModuleFileName(name); err == nil {
			t.Errorf("%s parsed without an error", name)
		}
	}
}

func TestCanonicalXMLIgnoresInsignificantDifferences(t *testing.T) {
	first := `<?xml version="1.0"?>
<nacm xmlns="urn:ietf:params:xml:ns:yang:ietf-netconf-acm">
  <!-- comment -->
  <enable-nacm>true</enable-nacm>
  <groups><group><name>a</name></group></groups>
</nacm>`
	second := `<n:nacm xmlns:n="urn:ietf:params:xml:ns:yang:ietf-netconf-acm"><n:enable-nacm> true </n:enable-nacm>` +
		`<n:groups><n:group><n:name>a</n:name></n:group></n:groups></n:nacm>`

	firstCanonical, err := sysrepo.CanonicalXML(first)
	if err != nil {
		t.Fatal(err)
	}
	secondCanonical, err := sysrepo.CanonicalXML(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstCanonical != secondCanonical {
		t.Errorf("canonical forms differ:\n%s\n%s", firstCanonical, secondCanonical)
	}
}

func TestCanonicalXMLDetectsDataDifferences(t *testing.T) {
	base := `<a xmlns="urn:x"><b>1</b></a>`
	variants := []string{
		`<a xmlns="urn:x"><b>2</b></a>`,
		`<a xmlns="urn:y"><b>1</b></a>`,
		`<a xmlns="urn:x"><c>1</c></a>`,
		`<a xmlns="urn:x"><b>1</b><b>1</b></a>`,
		``,
	}
	baseCanonical, err := sysrepo.CanonicalXML(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range variants {
		canonical, err := sysrepo.CanonicalXML(variant)
		if err != nil {
			t.Fatal(err)
		}
		if canonical == baseCanonical {
			t.Errorf("%q has the canonical form of %q", variant, base)
		}
	}
}

func TestCanonicalXMLRejectsMalformedDocument(t *testing.T) {
	_, err := sysrepo.CanonicalXML(`<a><b></a>`)
	if !errors.Is(err, sysrepo.ErrMalformedXML) {
		t.Errorf("error is %v, want ErrMalformedXML", err)
	}
}

func TestCanonicalXMLOfWhitespaceOnlyDocumentIsEmpty(t *testing.T) {
	canonical, err := sysrepo.CanonicalXML("  \n")
	if err != nil {
		t.Fatal(err)
	}
	if canonical != "" {
		t.Errorf("canonical form is %q, want empty", canonical)
	}
}

package sysrepo

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

const (
	listHeaderPrefix    = "Module Name"
	listColumnCount     = 7
	listColumnSeparator = "|"
	listNameColumn      = 0
	listRevisionColumn  = 1
	listFlagsColumn     = 2
	listFeaturesColumn  = 6

	// Uppercase I identifies an implemented module. Lowercase i identifies
	// a module that sysrepo imports but does not implement.
	implementedFlag = "\x49"

	moduleFileSuffixText = ".yang"
)

// ErrNoModuleTable marks sysrepoctl output without the module table.
var ErrNoModuleTable = errors.New("the sysrepoctl --list output has no module table")

var moduleFilePattern = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_.-]*)@([0-9]{4}-[0-9]{2}-[0-9]{2})\.yang$`)

// ModuleFile identifies the module and revision encoded in a YANG file name.
type ModuleFile struct {
	Module   string
	Revision string
}

// ParseModuleFileName requires <module>@<YYYY-MM-DD>.yang.
func ParseModuleFileName(filePath string) (ModuleFile, error) {
	base := path.Base(filePath)
	match := moduleFilePattern.FindStringSubmatch(base)
	if match == nil {
		return ModuleFile{}, fmt.Errorf(
			"the file name %q must have the form <module>@<revision>%s with a revision date such as 2018-02-20",
			base, moduleFileSuffixText,
		)
	}
	return ModuleFile{Module: match[1], Revision: match[2]}, nil
}

// InstalledModule records a module revision, implementation state, and enabled features.
type InstalledModule struct {
	Name     string
	Revision string
	// Implemented is true for a module that sysrepo installed. A module that
	// another module only imports is not implemented.
	Implemented bool
	// Features lists enabled features in sysrepoctl output order.
	Features []string
}

// ParseModuleList expects the seven-column sysrepoctl table and skips submodules.
func ParseModuleList(output string) ([]InstalledModule, error) {
	var modules []InstalledModule
	inTable := false
	for line := range strings.SplitSeq(output, "\n") {
		if !inTable {
			inTable = strings.HasPrefix(line, listHeaderPrefix)
			continue
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		if strings.Trim(line, "-") == "" {
			continue
		}
		columns := strings.Split(line, listColumnSeparator)
		if len(columns) != listColumnCount {
			return nil, fmt.Errorf("unexpected sysrepoctl --list row %q", line)
		}
		name := columns[listNameColumn]
		isSubmodule := strings.HasPrefix(name, " ")
		if isSubmodule {
			continue
		}
		flags := strings.TrimSpace(columns[listFlagsColumn])
		modules = append(modules, InstalledModule{
			Name:        strings.TrimSpace(name),
			Revision:    strings.TrimSpace(columns[listRevisionColumn]),
			Implemented: strings.HasPrefix(flags, implementedFlag),
			Features:    strings.Fields(columns[listFeaturesColumn]),
		})
	}
	if !inTable {
		return nil, ErrNoModuleTable
	}
	return modules, nil
}

// FindImplemented returns false for absent modules and modules that sysrepo only imports.
func FindImplemented(modules []InstalledModule, name string) (InstalledModule, bool) {
	for _, module := range modules {
		if module.Name == name && module.Implemented {
			return module, true
		}
	}
	return InstalledModule{}, false
}

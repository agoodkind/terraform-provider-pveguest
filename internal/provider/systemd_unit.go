package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

type unitFileState string

const (
	unitFileEnabled        unitFileState = "enabled"
	unitFileEnabledRuntime unitFileState = "enabled-runtime"
	unitFileStatic         unitFileState = "static"
	unitFileAlias          unitFileState = "alias"
	unitFileIndirect       unitFileState = "indirect"
	unitFileGenerated      unitFileState = "generated"
	unitFileTransient      unitFileState = "transient"
	unitFileMasked         unitFileState = "masked"
	unitFileMaskedRuntime  unitFileState = "masked-runtime"

	// systemd 253 and later print this state when the unit file does not
	// exist. Earlier versions print an error message.
	unitFileNotFound unitFileState = "not-found"
)

type unitActiveState string

const (
	unitActive     unitActiveState = "active"
	unitActivating unitActiveState = "activating"
	unitReloading  unitActiveState = "reloading"
	unitInactive   unitActiveState = "inactive"
	unitFailed     unitActiveState = "failed"
)

// enablement classifies the output of systemctl is-enabled.
type enablement int

const (
	enablementDisabled enablement = iota
	enablementEnabled
	// systemd reports a fixed unit as static, alias, indirect, generated, or
	// transient. A fixed unit satisfies both values of enabled.
	enablementFixed
)

type unitStatus struct {
	Found     bool
	FileState unitFileState
	Active    unitActiveState
}

// unitAction is a systemctl verb. daemon-reload does not take a unit name.
type unitAction string

const (
	unitActionDaemonReload unitAction = "daemon-reload"
	unitActionEnable       unitAction = "enable"
	unitActionReenable     unitAction = "reenable"
	unitActionDisable      unitAction = "disable"
	unitActionStart        unitAction = "start"
	unitActionStop         unitAction = "stop"
	unitActionRestart      unitAction = "restart"
)

// unitTarget reads unit status and runs unit actions on one guest or one
// host artifact target.
type unitTarget interface {
	// location returns the text that error messages use for the guest or the
	// host target.
	location() string
	readUnit(ctx context.Context, name string) (unitStatus, error)
	runUnitAction(ctx context.Context, action unitAction, name string) error
}

// reconcileUnit enables or disables the unit and starts, restarts, or stops
// it to match the declared values. restart reenables an enabled unit and
// restarts an active unit.
func reconcileUnit(
	ctx context.Context,
	target unitTarget,
	name string,
	status unitStatus,
	wantEnabled bool,
	wantActive bool,
	restart bool,
) error {
	current := classifyEnabledState(status.FileState)
	if current == enablementDisabled && wantEnabled {
		if err := target.runUnitAction(ctx, unitActionEnable, name); err != nil {
			return err
		}
	}
	// Rebuild installation links after dependency changes.
	if current == enablementEnabled && wantEnabled && restart {
		if err := target.runUnitAction(ctx, unitActionReenable, name); err != nil {
			return err
		}
	}
	if current == enablementEnabled && !wantEnabled {
		if err := target.runUnitAction(ctx, unitActionDisable, name); err != nil {
			return err
		}
	}

	isActive := isActiveState(status.Active)
	var action unitAction
	switch {
	case wantActive && !isActive:
		action = unitActionStart
	case wantActive && restart:
		action = unitActionRestart
	case !wantActive && isActive:
		action = unitActionStop
	default:
		return nil
	}
	return target.runUnitAction(ctx, action, name)
}

// removeUnit stops an active unit, disables an enabled unit, and verifies
// both results. An absent unit satisfies removal.
func removeUnit(ctx context.Context, target unitTarget, name string) error {
	status, err := target.readUnit(ctx, name)
	if err != nil {
		return err
	}
	if !status.Found {
		return nil
	}

	if !isStoppedState(status.Active) {
		if err := target.runUnitAction(ctx, unitActionStop, name); err != nil {
			return err
		}
	}
	if classifyEnabledState(status.FileState) == enablementEnabled {
		if err := target.runUnitAction(ctx, unitActionDisable, name); err != nil {
			return err
		}
	}

	after, err := target.readUnit(ctx, name)
	if err != nil {
		return err
	}
	if !after.Found {
		return nil
	}
	if !isStoppedState(after.Active) {
		return fmt.Errorf("on %s, unit %s has active state %s after stop; the active state must be inactive or failed", target.location(), name, after.Active)
	}
	if classifyEnabledState(after.FileState) == enablementEnabled {
		return fmt.Errorf("on %s, unit %s has unit file state %s after disable; the unit file state must not be enabled", target.location(), name, after.FileState)
	}
	return nil
}

// observedUnitValues returns the enabled and active values that Read writes to
// state. For a fixed unit, the function returns the enabled argument.
func observedUnitValues(status unitStatus, enabled types.Bool) (types.Bool, types.Bool) {
	switch classifyEnabledState(status.FileState) {
	case enablementEnabled:
		enabled = types.BoolValue(true)
	case enablementDisabled:
		enabled = types.BoolValue(false)
	case enablementFixed:
	}
	return enabled, types.BoolValue(isActiveState(status.Active))
}

func classifyEnabledState(state unitFileState) enablement {
	switch state {
	case unitFileEnabled, unitFileEnabledRuntime:
		return enablementEnabled
	case unitFileStatic, unitFileAlias, unitFileIndirect, unitFileGenerated, unitFileTransient:
		return enablementFixed
	case unitFileNotFound, unitFileMasked, unitFileMaskedRuntime:
		return enablementDisabled
	default:
		return enablementDisabled
	}
}

func isStoppedState(state unitActiveState) bool {
	return state == unitInactive || state == unitFailed
}

func isActiveState(state unitActiveState) bool {
	switch state {
	case unitActive, unitActivating, unitReloading:
		return true
	case unitInactive, unitFailed:
		return false
	default:
		return false
	}
}

package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"tamiops/internal/storage"
)

const connectionCapabilityVersion = 1

const (
	connectionProfileConditional = "conditional"
	connectionProfileCompatible  = "compatible"
	connectionProfileReadChecked = "read-checked"
	connectionProfileLimited     = "limited"
	connectionProfileFailed      = "failed"
)

const connectionDetectionTimeout = 30 * time.Second

// connectionDetection is the measured result used when adding or changing a
// connection. The profile records what was actually verified; it never turns
// an unsupported conditional request into an enabled conditional capability.
type connectionDetection struct {
	Capabilities          storage.Capabilities `json:"capabilities"`
	CompatibilityProfile  string               `json:"compatibilityProfile"`
	CapabilityVersion     int                  `json:"capabilityVersion"`
	CapabilitiesCheckedAt string               `json:"capabilitiesCheckedAt"`
	WriteRestriction      string               `json:"writeRestriction,omitempty"`
	ProbeWarning          string               `json:"probeWarning,omitempty"`
	ProbeUnsupported      bool                 `json:"probeUnsupported,omitempty"`
}

func applyConnectionDetection(c *Connection, result connectionDetection) {
	c.Tested = result.CompatibilityProfile != connectionProfileFailed
	c.Capabilities = result.Capabilities
	c.CompatibilityProfile = result.CompatibilityProfile
	c.CapabilityVersion = result.CapabilityVersion
	c.CapabilitiesCheckedAt = result.CapabilitiesCheckedAt
	c.WriteRestriction = result.WriteRestriction
	c.Error = ""
}

func invalidateConnectionDetection(c *Connection, message string) {
	c.Tested = false
	c.Capabilities = storage.Capabilities{}
	c.CompatibilityProfile = connectionProfileFailed
	c.CapabilityVersion = connectionCapabilityVersion
	c.CapabilitiesCheckedAt = time.Now().UTC().Format(time.RFC3339Nano)
	c.Error = message
	c.WriteRestriction = ""
	c.Detecting = false
}

func connectionDetectionCurrent(c Connection) bool {
	if c.CapabilityVersion != connectionCapabilityVersion || c.CapabilitiesCheckedAt == "" {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, c.CapabilitiesCheckedAt); err != nil {
		return false
	}
	if c.CompatibilityProfile != connectionProfileFailed && !c.Tested {
		return false
	}
	switch c.CompatibilityProfile {
	case connectionProfileConditional:
		return c.Capabilities.ConditionalWrite && c.Capabilities.ConditionalDelete && c.WriteRestriction == ""
	case connectionProfileCompatible:
		return !c.Capabilities.ConditionalWrite && !c.Capabilities.ConditionalDelete && !c.Capabilities.MultipartConditional && c.WriteRestriction != ""
	case connectionProfileReadChecked:
		return !c.Capabilities.ConditionalWrite && !c.Capabilities.ConditionalDelete && !c.Capabilities.MultipartConditional
	case connectionProfileLimited:
		return !c.Capabilities.ConditionalWrite && !c.Capabilities.ConditionalDelete && !c.Capabilities.MultipartConditional && c.WriteRestriction != ""
	case connectionProfileFailed:
		return !c.Tested && c.Capabilities == (storage.Capabilities{}) && c.Error != ""
	default:
		return false
	}
}

func (d connectionDetection) current() bool {
	c := Connection{
		Capabilities:          d.Capabilities,
		CompatibilityProfile:  d.CompatibilityProfile,
		CapabilityVersion:     d.CapabilityVersion,
		CapabilitiesCheckedAt: d.CapabilitiesCheckedAt,
		Tested:                d.CompatibilityProfile != connectionProfileFailed,
		Error:                 d.ProbeWarning,
		WriteRestriction:      d.WriteRestriction,
	}
	if !connectionDetectionCurrent(c) {
		return false
	}
	if d.CompatibilityProfile == connectionProfileCompatible {
		return d.ProbeUnsupported
	}
	if d.CompatibilityProfile == connectionProfileLimited {
		return !d.ProbeUnsupported
	}
	return !d.ProbeUnsupported
}

// detectConnectionCapabilities first proves that the configured scope can be
// listed, then rechecks readability after a failed write probe. A failed read
// check or cancelled attempt prevents saving; a write-only failure is retained
// as a restriction so the selected policy can enforce its normal safeguards.
func detectConnectionCapabilities(parent context.Context, store storage.Store, writeMode string, probeWrites bool) (connectionDetection, error) {
	result := connectionDetection{
		CapabilityVersion:     connectionCapabilityVersion,
		CapabilitiesCheckedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if store == nil {
		result.CompatibilityProfile = connectionProfileFailed
		return result, errors.New("storage is nil")
	}
	ctx, cancel := context.WithTimeout(parent, connectionDetectionTimeout)
	defer cancel()
	if _, err := store.List(ctx, ""); err != nil {
		result.CompatibilityProfile = connectionProfileFailed
		result.ProbeWarning = err.Error()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return result, err
		}
		return result, fmt.Errorf("连接测试失败：%w", err)
	}
	if !probeWrites || writeMode == storage.WriteModeCopy {
		result.CompatibilityProfile = connectionProfileReadChecked
		result.Capabilities.RangeRead = probeReadRange(ctx, store)
		if ctx.Err() != nil {
			result.CompatibilityProfile = connectionProfileFailed
			result.ProbeWarning = ctx.Err().Error()
			return result, ctx.Err()
		}
		return result, nil
	}

	caps, probeErr := storage.Probe(ctx, store)
	if ctx.Err() != nil {
		result.CompatibilityProfile = connectionProfileFailed
		result.ProbeWarning = ctx.Err().Error()
		return result, ctx.Err()
	}
	if probeErr == nil {
		result.Capabilities = caps
		result.CompatibilityProfile = connectionProfileConditional
		return result, nil
	}
	// A failed write probe can also mean that authentication or reachability
	// changed while it ran. Confirm readable access again before persisting a
	// capability restriction.
	if _, err := store.List(ctx, ""); err != nil {
		result.CompatibilityProfile = connectionProfileFailed
		result.Capabilities = storage.Capabilities{}
		result.ProbeWarning = probeErr.Error()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return result, err
		}
		return result, fmt.Errorf("连接测试失败：%w", err)
	}
	if ctx.Err() != nil {
		result.CompatibilityProfile = connectionProfileFailed
		result.ProbeWarning = ctx.Err().Error()
		return result, ctx.Err()
	}
	result.Capabilities = storage.Capabilities{RangeRead: probeReadRange(ctx, store)}
	if ctx.Err() != nil {
		result.CompatibilityProfile = connectionProfileFailed
		result.Capabilities = storage.Capabilities{}
		result.ProbeWarning = ctx.Err().Error()
		return result, ctx.Err()
	}
	result.ProbeWarning = probeErr.Error()
	result.WriteRestriction = probeErr.Error()
	result.ProbeUnsupported = errors.Is(probeErr, storage.ErrConditionalUnsupported)
	if result.ProbeUnsupported {
		result.CompatibilityProfile = connectionProfileCompatible
	} else {
		result.CompatibilityProfile = connectionProfileLimited
	}
	return result, nil
}

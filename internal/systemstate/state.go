// Package systemstate reads the small set of host facts used by automatic
// execution rules. Unknown power state is represented explicitly.
package systemstate

import (
	"context"
	"net"
	"sort"
)

type Snapshot struct {
	NetworkInterfaces []string
	OnAC              bool
	PowerKnown        bool
}

type snapshotKey struct{}

// WithSnapshot injects a deterministic state snapshot into a context. It is
// intended for tests and internal callers that already own a trusted snapshot.
func WithSnapshot(ctx context.Context, snapshot Snapshot) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot.NetworkInterfaces = append([]string(nil), snapshot.NetworkInterfaces...)
	return context.WithValue(ctx, snapshotKey{}, snapshot)
}

func Read(ctx context.Context) Snapshot {
	if ctx == nil {
		ctx = context.Background()
	}
	if snapshot, ok := ctx.Value(snapshotKey{}).(Snapshot); ok {
		snapshot.NetworkInterfaces = append([]string(nil), snapshot.NetworkInterfaces...)
		return snapshot
	}
	onAC, powerKnown := onAC(ctx)
	return Snapshot{NetworkInterfaces: activeNetworkInterfaces(), OnAC: onAC, PowerKnown: powerKnown}
}

func activeNetworkInterfaces() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(interfaces))
	seen := make(map[string]bool, len(interfaces))
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagRunning == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Name == "" || seen[iface.Name] {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil || len(addresses) == 0 {
			continue
		}
		seen[iface.Name] = true
		names = append(names, iface.Name)
	}
	sort.Strings(names)
	return names
}

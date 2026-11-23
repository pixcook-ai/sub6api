//go:build !bpssdkfixture

package service

import (
	"context"
	"net"
	"net/netip"
	"time"
)

// Ordinary builds always use the system resolver and a public-address-pinned
// dialer. The isolated SDK fixture configuration does not exist in this build.
func defaultManagedFileURLNetwork() managedFileURLNetwork {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return managedFileURLNetwork{
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial: dialer.DialContext,
	}
}

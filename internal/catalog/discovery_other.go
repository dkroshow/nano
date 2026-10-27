//go:build !darwin

package catalog

import "errors"

// Discovery is unavailable off macOS: sessions Nano did not start enter the
// catalog only through Explorer's Refresh.
type Discovery struct{}

func StartDiscovery([]string, func([]string), func(string, ...any)) (*Discovery, error) {
	return nil, errors.New("outside-session discovery requires macOS FSEvents")
}

func (*Discovery) Close() {}

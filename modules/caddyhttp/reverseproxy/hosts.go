// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package reverseproxy

import (
	"fmt"
	"sync/atomic"

	"github.com/caddyserver/caddy/v2"
)

// Host represents a remote host which can be proxied to.
// Its methods must be safe for concurrent use.
type Host interface {
	// NumRequests returns the numnber of requests
	// currently in process with the host.
	NumRequests() int

	// Fails returns the count of recent failures.
	Fails() int

	// Unhealthy returns true if the backend is unhealthy.
	Unhealthy() bool

	// CountRequest atomically counts the given number of
	// requests as currently in process with the host. The
	// count should not go below 0.
	CountRequest(int) error

	// CountFail atomically counts the given number of
	// failures with the host. The count should not go
	// below 0.
	CountFail(int) error

	// SetHealthy atomically marks the host as either
	// healthy (true) or unhealthy (false). If the given
	// status is the same, this should be a no-op and
	// return false. It returns true if the status was
	// changed; i.e. if it is now different from before.
	SetHealthy(bool) (bool, error)
}

// UpstreamPool is a collection of upstreams.
type UpstreamPool []*Upstream

// Upstream bridges this proxy's configuration to the
// state of the backend host it is correlated with.
type Upstream struct {
	Host `json:"-"`

	Dial        string `json:"dial,omitempty"`
	MaxRequests int    `json:"max_requests,omitempty"`

	// TODO: This could be really useful, to bind requests
	// with certain properties to specific backends
	// HeaderAffinity string
	// IPAffinity     string

	healthCheckPolicy *PassiveHealthChecks
	cb                CircuitBreaker
	dialInfo          DialInfo
}

// Available returns true if the remote host
// is available to receive requests. This is
// the method that should be used by selection
// policies, etc. to determine if a backend
// should be able to be sent a request.
func (u *Upstream) Available() bool {
	return u.Healthy() && !u.Full()
}

// Healthy returns true if the remote host
// is currently known to be healthy or "up".
// It consults the circuit breaker, if any.
func (u *Upstream) Healthy() bool {
	healthy := !u.Host.Unhealthy()
	if healthy && u.healthCheckPolicy != nil {
		healthy = u.Host.Fails() < u.healthCheckPolicy.MaxFails
	}
	if healthy && u.cb != nil {
		healthy = u.cb.OK()
	}
	return healthy
}

// Full returns true if the remote host
// cannot receive more requests at this time.
func (u *Upstream) Full() bool {
	return u.MaxRequests > 0 && u.Host.NumRequests() >= u.MaxRequests
}

// upstreamHost is the basic, in-memory representation
// of the state of a remote host. It implements the
// Host interface.
type upstreamHost struct {
	numRequests int64 // must be first field to be 64-bit aligned on 32-bit systems (see https://golang.org/pkg/sync/atomic/#pkg-note-BUG)
	fails       int64
	unhealthy   int32
}

// NumRequests returns the number of active requests to the upstream.
func (uh *upstreamHost) NumRequests() int {
	return int(atomic.LoadInt64(&uh.numRequests))
}

// Fails returns the number of recent failures with the upstream.
func (uh *upstreamHost) Fails() int {
	return int(atomic.LoadInt64(&uh.fails))
}

// Unhealthy returns whether the upstream is healthy.
func (uh *upstreamHost) Unhealthy() bool {
	return atomic.LoadInt32(&uh.unhealthy) == 1
}

// CountRequest mutates the active request count by
// delta. It returns an error if the adjustment fails.
func (uh *upstreamHost) CountRequest(delta int) error {
	result := atomic.AddInt64(&uh.numRequests, int64(delta))
	if result < 0 {
		return fmt.Errorf("count below 0: %d", result)
	}
	return nil
}

// CountFail mutates the recent failures count by
// delta. It returns an error if the adjustment fails.
func (uh *upstreamHost) CountFail(delta int) error {
	result := atomic.AddInt64(&uh.fails, int64(delta))
	if result < 0 {
		return fmt.Errorf("count below 0: %d", result)
	}
	return nil
}

// SetHealthy sets the upstream has healthy or unhealthy
// and returns true if the value was different from before,
// or an error if the adjustment failed.
func (uh *upstreamHost) SetHealthy(healthy bool) (bool, error) {
	var unhealthy, compare int32 = 1, 0
	if healthy {
		unhealthy, compare = 0, 1
	}
	swapped := atomic.CompareAndSwapInt32(&uh.unhealthy, compare, unhealthy)
	return swapped, nil
}

// DialInfo contains information needed to dial a
// connection to an upstream host. This information
// may be different than that which is represented
// in a URL (for example, unix sockets don't have
// a host that can be represented in a URL, but
// they certainly have a network name and address).
type DialInfo struct {
	// The network to use. This should be one of the
	// values that is accepted by net.Dial:
	// https://golang.org/pkg/net/#Dial
	Network string

	// The address to dial. Follows the same
	// semantics and rules as net.Dial.
	Address string
}

// String returns the Caddy network address form
// by joining the network and address with a
// forward slash.
func (di DialInfo) String() string {
	return di.Network + "/" + di.Address
}

// DialInfoCtxKey is used to store a DialInfo
// in a context.Context.
const DialInfoCtxKey = caddy.CtxKey("dial_info")

// hosts is the global repository for hosts that are
// currently in use by active configuration(s). This
// allows the state of remote hosts to be preserved
// through config reloads.
var hosts = caddy.NewUsagePool()
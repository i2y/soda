// The public API of this file (vmsPool, newPool, run) follows the
// shape of github.com/pocketbase/pocketbase/plugins/jsvm/pool.go so
// Soda's internal callers can mirror the upstream layout without
// rename. The internals — buffered-channel availability tracking and
// the all slice backing forEach — are a Soda-specific rewrite that
// differs structurally from upstream's mutex + busy-flag approach.
//
// Upstream shape: Copyright (c) 2022 - present, Gani Georgiev. MIT License.
// https://github.com/pocketbase/pocketbase/blob/master/LICENSE.md
// Rewrite: Copyright (c) 2026 - present, Yasushi Itoh.

package soda

import "github.com/i2y/ramune"

// vmsPool hands out pre-warmed Ramune runtimes to callers that need an
// isolated JS VM per request (hook dispatch, router handler, cron
// tick). Acquisition goes through a buffered channel so there is no
// explicit locking and no hand-rolled "busy" bookkeeping — a VM is
// either in the channel (available) or with a caller (checked out).
//
// The all slice keeps ownership of every pre-warmed VM even when it's
// checked out, so pool-wide operations (e.g. adding extra bindings
// after construction) can iterate deterministically. all is written
// only during newPool and read afterward; callers using forEach must
// do so before traffic starts.
//
// When the pool is exhausted — all pre-warmed VMs currently in flight —
// the pool falls back to spawning a one-off VM that is closed when the
// call returns. That keeps hook dispatch from stalling under bursts at
// the cost of the VM warmup time for the overflow request.
type vmsPool struct {
	factory func() *ramune.Runtime
	free    chan *ramune.Runtime
	all     []*ramune.Runtime
}

// newPool returns a pool primed with size pre-warmed runtimes.
func newPool(size int, factory func() *ramune.Runtime) *vmsPool {
	p := &vmsPool{
		factory: factory,
		free:    make(chan *ramune.Runtime, size),
		all:     make([]*ramune.Runtime, 0, size),
	}
	for i := 0; i < size; i++ {
		vm := factory()
		p.all = append(p.all, vm)
		p.free <- vm
	}
	return p
}

// run checks out a runtime, invokes call with it, and returns it to
// the pool after call completes (on success, error, or panic via
// defer). When every pooled runtime is in use a disposable one is
// created and Close'd around call instead.
func (p *vmsPool) run(call func(vm *ramune.Runtime) error) error {
	select {
	case vm := <-p.free:
		defer func() { p.free <- vm }()
		return call(vm)
	default:
		vm := p.factory()
		defer vm.Close()
		return call(vm)
	}
}

// forEach applies fn to every pre-warmed runtime owned by the pool.
// Intended for one-shot, pre-traffic setup such as registering extra
// native bindings after newPool; not safe to call while the pool is
// serving concurrent run() calls that might mutate VM state.
func (p *vmsPool) forEach(fn func(*ramune.Runtime)) {
	for _, vm := range p.all {
		fn(vm)
	}
}

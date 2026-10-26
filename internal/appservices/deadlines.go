package appservices

import "time"

const (
	// Read-only status never waits for asynchronous native shutdown.
	NativeStatusTimeout = 5 * time.Second
	// The bridge owns a 30-second completion bound plus process-start margin.
	NativeCommandTimeout = 35 * time.Second
	// A phone restart can unregister and register, query status four times,
	// and spend 30 seconds on owned-process shutdown and readiness.
	ControllerLifecycleTimeout = 2*NativeCommandTimeout + 4*NativeStatusTimeout + 30*time.Second
	ControllerCommandTimeout   = ControllerLifecycleTimeout + 5*time.Second
)

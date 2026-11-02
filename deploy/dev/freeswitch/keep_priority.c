// SPDX-License-Identifier: Apache-2.0
//
// Keeps a native macOS FreeSWITCH from lowering its own threads' priority (#451).
//
// FreeSWITCH creates every thread through switch_threadattr_create, which asks
// for the lowest priority (1) for most of them, and APR applies that request
// with pthread_setschedparam from inside the new thread. Linux ignores it.
// macOS applies it, and a thread below priority 4 is placed in the throttled
// tier, which runs only on the efficiency cores. Anything that keeps those
// cores busy (make test-stress does) then starves ESL and the audio threads.
//
// This library interposes pthread_setschedparam and drops a call that would
// lower the thread's current priority. The thread keeps the default, so it can
// run on any core. A call that raises or keeps the priority passes through
// unchanged.
//
// It also interposes sched_yield. On macOS that call depresses the caller's
// priority for a scheduling quantum; on Linux it only lets the thread's peers
// run first. FreeSWITCH yields after every byte it reads from an ESL socket
// (read_packet in mod_event_socket), so with the efficiency cores busy each
// command waited behind the load once per byte. The replacement yields with
// thread_switch(SWITCH_OPTION_NONE), which does not depress the priority.
//
// It changes nothing else and logs nothing per call.
//
// Build and install: deploy/dev/freeswitch/install.sh builds it into
// $FS_ROOT/lib/libaicc_keep_priority.dylib. Load: deploy/dev/freeswitch/start.sh
// starts the switch with DYLD_INSERT_LIBRARIES pointing at it.

#ifndef __APPLE__
#error "keep_priority.c is only for macOS: other systems ignore the request already"
#endif

#include <mach/mach.h>
#include <mach/thread_switch.h>
#include <pthread.h>
#include <sched.h>
#include <stdio.h>

static int keep_priority(pthread_t thread, int policy, const struct sched_param *param) {
	int current_policy;
	struct sched_param current;

	if (param != NULL && pthread_getschedparam(thread, &current_policy, &current) == 0 &&
	    param->sched_priority < current.sched_priority) {
		return 0;
	}
	// Inside the interposing image this names the real function.
	return pthread_setschedparam(thread, policy, param);
}

static int yield_without_depression(void) {
	(void)thread_switch(MACH_PORT_NULL, SWITCH_OPTION_NONE, 0);
	return 0;
}

__attribute__((used, section("__DATA,__interpose"))) static struct {
	const void *replacement;
	const void *original;
} interpose[] = {
	{(const void *)keep_priority, (const void *)pthread_setschedparam},
	{(const void *)yield_without_depression, (const void *)sched_yield},
};

__attribute__((constructor)) static void announce(void) {
	fprintf(stderr, "keep_priority: FreeSWITCH threads keep the default priority (deploy/dev/freeswitch, #451)\n");
}

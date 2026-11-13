//go:build darwin && cgo

package daemon

/*
#cgo LDFLAGS: -framework CoreServices
#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>
#include <stdint.h>
#include <stdlib.h>

extern void goOmniaEvents(uintptr_t, size_t, void *, void *);
typedef struct { FSEventStreamRef stream; dispatch_queue_t queue; } omnia_watch;

static void omnia_callback(ConstFSEventStreamRef stream, void *info, size_t n, void *paths,
                           const FSEventStreamEventFlags flags[], const FSEventStreamEventId ids[]) {
    goOmniaEvents((uintptr_t)info, n, paths, (void *)flags);
}
static omnia_watch *omnia_watch_start(uintptr_t handle, char **paths, int count) {
    CFMutableArrayRef roots = CFArrayCreateMutable(NULL, count, &kCFTypeArrayCallBacks);
    for (int i = 0; i < count; i++) {
        CFStringRef path = CFStringCreateWithCString(NULL, paths[i], kCFStringEncodingUTF8);
        if (!path) { CFRelease(roots); return NULL; }
        CFArrayAppendValue(roots, path);
        CFRelease(path);
    }
    omnia_watch *w = calloc(1, sizeof(omnia_watch));
    if (!w) { CFRelease(roots); return NULL; }
    FSEventStreamContext context = {0, (void *)handle, NULL, NULL, NULL};
    w->stream = FSEventStreamCreate(NULL, omnia_callback, &context, roots,
        kFSEventStreamEventIdSinceNow, 0.1,
        kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagNoDefer | kFSEventStreamCreateFlagWatchRoot);
    CFRelease(roots);
    if (!w->stream) { free(w); return NULL; }
    w->queue = dispatch_queue_create("omnia.filesystem", DISPATCH_QUEUE_SERIAL);
    FSEventStreamSetDispatchQueue(w->stream, w->queue);
    if (!FSEventStreamStart(w->stream)) {
        FSEventStreamInvalidate(w->stream); FSEventStreamRelease(w->stream);
        dispatch_release(w->queue); free(w); return NULL;
    }
    return w;
}
static void omnia_barrier(void *unused) {}
static void omnia_watch_stop(omnia_watch *w) {
    FSEventStreamStop(w->stream);
    FSEventStreamInvalidate(w->stream);
    dispatch_sync_f(w->queue, NULL, omnia_barrier);
    FSEventStreamRelease(w->stream);
    dispatch_release(w->queue);
    free(w);
}
*/
import "C"

import (
	"fmt"
	"runtime/cgo"
	"unsafe"

	"github.com/wiruzman/omnia/internal/metrics"
)

func startNativeWatcher(roots []string, w *fileWatcher) (func(), error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("no filesystem roots configured")
	}
	paths := make([]*C.char, len(roots))
	for i, root := range roots {
		paths[i] = C.CString(root)
		defer C.free(unsafe.Pointer(paths[i]))
	}
	handle := cgo.NewHandle(w)
	native := C.omnia_watch_start(C.uintptr_t(handle), &paths[0], C.int(len(paths)))
	if native == nil {
		handle.Delete()
		return nil, fmt.Errorf("start filesystem event stream")
	}
	return func() { C.omnia_watch_stop(native); handle.Delete() }, nil
}

//export goOmniaEvents
func goOmniaEvents(handle C.uintptr_t, n C.size_t, paths unsafe.Pointer, flags unsafe.Pointer) {
	w := cgo.Handle(handle).Value().(*fileWatcher)
	pathList := unsafe.Slice((**C.char)(paths), int(n))
	flagList := unsafe.Slice((*C.uint32_t)(flags), int(n))
	for i, path := range pathList {
		w.nativeEvent(C.GoString(path), uint32(flagList[i]))
	}
}

func (w *fileWatcher) nativeEvent(name string, flag uint32) {
	for _, reason := range []struct {
		flag   uint32
		reason metrics.RecoveryReason
	}{
		{C.kFSEventStreamEventFlagMustScanSubDirs, metrics.NativeMustScan},
		{C.kFSEventStreamEventFlagUserDropped, metrics.NativeUserDropped},
		{C.kFSEventStreamEventFlagKernelDropped, metrics.NativeKernelDropped},
		{C.kFSEventStreamEventFlagEventIdsWrapped, metrics.NativeIDsWrapped},
		{C.kFSEventStreamEventFlagRootChanged, metrics.NativeRootChanged},
		{C.kFSEventStreamEventFlagMount, metrics.NativeMount},
		{C.kFSEventStreamEventFlagUnmount, metrics.NativeUnmount},
	} {
		if flag&reason.flag != 0 {
			w.metrics.RecoveryEvent(reason.reason)
		}
	}
	// Actual loss invalidates the stream, regardless of the reported path.
	if flag&(C.kFSEventStreamEventFlagUserDropped|C.kFSEventStreamEventFlagKernelDropped|C.kFSEventStreamEventFlagEventIdsWrapped) != 0 {
		w.lost()
		return
	}
	if flag&(C.kFSEventStreamEventFlagMustScanSubDirs|C.kFSEventStreamEventFlagRootChanged|C.kFSEventStreamEventFlagMount|C.kFSEventStreamEventFlagUnmount) != 0 {
		w.recoverPath(name)
		return
	}
	if flag&(C.kFSEventStreamEventFlagItemCreated|C.kFSEventStreamEventFlagItemRemoved|C.kFSEventStreamEventFlagItemRenamed|C.kFSEventStreamEventFlagItemModified|C.kFSEventStreamEventFlagItemInodeMetaMod) == 0 {
		return
	}
	// Descendants of an incoming directory may have no individual events.
	// Discover that subtree through bounded pending work, not a global rescan.
	discover := flag&C.kFSEventStreamEventFlagItemIsDir != 0 && flag&(C.kFSEventStreamEventFlagItemCreated|C.kFSEventStreamEventFlagItemRenamed) != 0
	w.publishEvent(fileEvent{path: name, discover: discover})
}

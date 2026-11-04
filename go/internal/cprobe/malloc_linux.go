package cprobe

/*
#include <stdlib.h>

#if defined(__GLIBC__) && (__GLIBC__ > 2 || (__GLIBC__ == 2 && __GLIBC_MINOR__ >= 33))
#include <malloc.h>

static int cprobe_malloc_in_use(size_t *out) {
	struct mallinfo2 info = mallinfo2();
	*out = info.uordblks + info.hblkhd;
	return 1;
}
#else
static int cprobe_malloc_in_use(size_t *out) {
	(void)out;
	return 0;
}
#endif
*/
import "C"

// Unit names what MallocInUse counts, and MinLeak is the least one leaked allocation adds:
// glibc's smallest chunk is 16 bytes on 32-bit targets and 32 on 64-bit ones.
const (
	Unit    = "bytes"
	MinLeak = 16
)

// MallocInUse returns the bytes held by live heap allocations. It reports false
// unless the C library is glibc 2.33 or later.
func MallocInUse() (uint64, bool) {
	var n C.size_t
	ok := C.cprobe_malloc_in_use(&n) != 0
	return uint64(n), ok
}

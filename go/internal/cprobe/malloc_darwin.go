package cprobe

/*
#include <malloc/malloc.h>

static size_t cprobe_malloc_in_use(void) {
	malloc_statistics_t stats;
	malloc_zone_statistics(NULL, &stats);
	return stats.blocks_in_use;
}
*/
import "C"

// Unit names what MallocInUse counts, and MinLeak is the least one leaked allocation adds.
const (
	Unit    = "blocks"
	MinLeak = 1
)

// MallocInUse returns the number of live C heap blocks across all malloc zones.
func MallocInUse() (uint64, bool) {
	return uint64(C.cprobe_malloc_in_use()), true
}

// Copyright 2017 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

#include <errno.h>
#include <pthread.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <zircon/process.h>
#include <zircon/status.h>
#include <zircon/syscalls.h>

#define MAX_COVSZ (1ULL << 20)

// In x86_64, sancov stores the return address - 1.
// We add 1 so the stored value points to a valid pc.
static const uint64_t kPcFixup = 1;

static const char* kCovPcsFileName = "/boot/kernel/data/zircon.elf.1.sancov";

typedef struct cover_ctx_t {
	uint64_t base_covcount[MAX_COVSZ];
	uint64_t curr_covcount[MAX_COVSZ];
	uint64_t pc_table[MAX_COVSZ];
	uint32_t real_coverage_truncated[MAX_COVSZ];
	size_t total_pcs; // number of elements in pc_table and covcount tables, determined by kernel.
	zx_handle_t covcount_vmo;
} cover_ctx;

static __thread cover_ctx cover;

static void cover_open(cover_t* cov, bool extra)
{
}

static void cover_enable(cover_t* cov, bool collect_comps, bool extra)
{
	zx_status_t status = zx_coverage_ctl(zx_thread_self(), 1, ZX_HANDLE_INVALID);
	if (status != ZX_OK) {
		fail("failed to enable coverage. err: %d\n", status);
	}

	status = zx_vmo_create(sizeof(cover.curr_covcount), 0, &cover.covcount_vmo);
	if (status != ZX_OK) {
		fail("failed to create covcount vmo. err: %d\n", status);
	}
}

static size_t snapshot_sancov(uint64_t* dst, size_t elems, const char* filename)
{
	FILE* f = fopen(filename, "rb");
	if (f == NULL) {
		fail("could not open coverage file '%s'", filename);
	}

	size_t n = fread(dst, sizeof(uint64_t), elems, f);
	if (n == elems) {
		fail("pc table is too small. make it bigger.");
	}

	fclose(f);
	return n;
}

static size_t snapshot_pctable(uint64_t* dst, size_t elems)
{
	return snapshot_sancov(dst, elems, kCovPcsFileName);
}

static void snapshot_covcount(uint64_t* dst, size_t elems)
{
	// TODO: Only read the right amount of coverage.
	zx_status_t status = zx_coverage_ctl(zx_thread_self(), 2, cover.covcount_vmo);
	if (status != ZX_OK) {
		fail("failed to fetch coverage. err: %d\n", status);
	}
	status = zx_vmo_read(cover.covcount_vmo, dst, 0, elems * sizeof(uint64_t));
	if (status != ZX_OK) {
		fail("failed to copy coverage. err: %d\n", status);
	}
}

static void cover_reset(cover_t* cov)
{
	snapshot_covcount(cover.base_covcount, MAX_COVSZ);
}

static void cover_collect(cover_t* cov)
{
	snapshot_covcount(cover.curr_covcount, MAX_COVSZ);
	size_t cov_size = snapshot_pctable(cover.pc_table, MAX_COVSZ);
	cover.total_pcs = cov_size;
	size_t num_pcs = 0;
	for (size_t i = 0; i < cov_size; i++) {
		if (cover.pc_table[i] == 0)
			continue;
		if (cover.base_covcount[i] == cover.curr_covcount[i])
			continue;

		cover.real_coverage_truncated[num_pcs] = static_cast<uint32_t>((cover.pc_table[i] + kPcFixup) & 0xFFFFFFFF);
		num_pcs += 1;
	}

	cov->size = num_pcs;
	cov->data = reinterpret_cast<char*>(cover.real_coverage_truncated);
}

static void cover_protect(cover_t* cov)
{
}

static void os_init(int argc, char** argv, void* data, size_t data_size)
{
	zx_status_t status = syz_mmap((size_t)data, data_size);
	if (status != ZX_OK)
		fail("mmap of data segment failed: %s (%d)", zx_status_get_string(status), status);
}

static intptr_t execute_syscall(const call_t* c, intptr_t a[kMaxArgs])
{
	intptr_t res = c->call(a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7], a[8]);
	if (strncmp(c->name, "zx_", 3) == 0) {
		// Convert zircon error convention to the libc convention that executor expects.
		// The following calls return arbitrary integers instead of error codes.
		if (res == ZX_OK ||
		    !strcmp(c->name, "zx_debuglog_read") ||
		    !strcmp(c->name, "zx_clock_get") ||
		    !strcmp(c->name, "zx_clock_get_monotonic") ||
		    !strcmp(c->name, "zx_deadline_after") ||
		    !strcmp(c->name, "zx_ticks_get"))
			return 0;
		errno = (-res) & 0x7f;
		return -1;
	}
	// We cast libc functions to signature returning intptr_t,
	// as the result int -1 is returned as 0x00000000ffffffff rather than full -1.
	if (res == 0xffffffff)
		res = (intptr_t)-1;
	return res;
}

void write_call_output(thread_t* th, bool finished)
{
	uint32 reserrno = 999;
	const bool blocked = th != last_scheduled;
	uint32 call_flags = call_flag_executed | (blocked ? call_flag_blocked : 0);
	if (finished) {
		reserrno = th->res != -1 ? 0 : th->reserrno;
		call_flags |= call_flag_finished |
			      (th->fault_injected ? call_flag_fault_injected : 0);
	}
	call_reply reply;
	reply.header.magic = kOutMagic;
	reply.header.done = 0;
	reply.header.status = 0;
	reply.call_index = th->call_index;
	reply.call_num = th->call_num;
	reply.reserrno = reserrno;
	reply.flags = call_flags;
	reply.signal_size = 0;
	reply.cover_size = 0;
	reply.comps_size = 0;
	if (flag_coverage) {
		reply.signal_size = th->cov.size;
		if (flag_collect_cover) {
			reply.cover_size = th->cov.size;
		}
	}
	if (write(kOutPipeFd, &reply, sizeof(reply)) != sizeof(reply))
		fail("control pipe call write failed");

	if (flag_coverage) {
		// In Fuchsia, coverage is collected by instrumenting edges instead of
		// basic blocks. This means that the signal that syzkaller
		// understands is the same as the coverage PCs.
		ssize_t wrote = write(kOutPipeFd, th->cov.data, th->cov.size * sizeof(uint32_t));
		if (wrote != sizeof(uint32_t) * th->cov.size) {
			fail("signals table write failed. Wrote %zd\n", wrote);
		}
		if (!flag_collect_cover) {
			return;
		}
		wrote = write(kOutPipeFd, th->cov.data, th->cov.size * sizeof(uint32_t));
		if (wrote != sizeof(uint32_t) * th->cov.size) {
			fail("coverage table write failed. Wrote %zd\n", wrote);
		}
	}

	debug_verbose("out: index=%u num=%u errno=%d finished=%d blocked=%d\n",
		      th->call_index, th->call_num, reserrno, finished, blocked);
}

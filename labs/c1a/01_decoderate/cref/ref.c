/* The C reference for the K2 assembly clause (doc 11): the same
 * LSB-first unpack loop hotfmt runs, compiled -O3 -march=native on the
 * gate box, so the 2.5x comparison is same-box and same-algorithm.
 * Output rows match the Go binary's TSV: label arm block width reps
 * postings M/s GBin/s GBout/s.
 *
 * Build and run (run.sh does both):
 *   cc -O3 -march=native -o cref/cref cref/ref.c && ./cref/cref server3 0.7
 * Lives in its own subdirectory because the Go toolchain refuses a .c
 * file inside a Go package without cgo, and chizu has no cgo.
 */
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#define WORK_TARGET (32u << 20) /* decoded uint32 bytes per pass */

static const unsigned width_menu[] = {1, 2, 3, 4, 5, 6, 7, 8, 10, 12, 16, 20, 24, 28, 32};
#define NWIDTHS (sizeof(width_menu) / sizeof(width_menu[0]))

static int packed_len(int n, unsigned w) { return (n * (int)w + 7) / 8; }

static void pack(uint8_t *dst, const uint32_t *vals, int n, unsigned w) {
    uint64_t acc = 0;
    unsigned nbits = 0;
    size_t pos = 0;
    for (int i = 0; i < n; i++) {
        acc |= (uint64_t)vals[i] << nbits;
        nbits += w;
        while (nbits >= 8) {
            dst[pos++] = (uint8_t)acc;
            acc >>= 8;
            nbits -= 8;
        }
    }
    if (nbits > 0)
        dst[pos] = (uint8_t)acc;
}

static void unpack(const uint8_t *data, uint32_t *vals, int n, unsigned w) {
    uint64_t acc = 0;
    unsigned nbits = 0;
    size_t pos = 0;
    uint64_t mask = (w == 32) ? 0xFFFFFFFFu : (((uint64_t)1 << w) - 1);
    for (int i = 0; i < n; i++) {
        while (nbits < w) {
            acc |= (uint64_t)data[pos++] << nbits;
            nbits += 8;
        }
        vals[i] = (uint32_t)(acc & mask);
        acc >>= w;
        nbits -= w;
    }
}

/* xorshift64, seeded like the Go side's fixed seed; the distributions
 * only need to be uniform within the width, not identical to Go's. */
static uint64_t rng_state = 2107;
static uint64_t rng_next(void) {
    uint64_t x = rng_state;
    x ^= x << 13;
    x ^= x >> 7;
    x ^= x << 17;
    return rng_state = x;
}

static double now_sec(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
}

int main(int argc, char **argv) {
    const char *label = argc > 1 ? argv[1] : "local";
    double budget = argc > 2 ? atof(argv[2]) : 0.7;
    static const int blocks[] = {64, 128, 256};
    for (size_t bi = 0; bi < 3; bi++) {
        int block = blocks[bi];
        int nblocks = (int)(WORK_TARGET / 4 / (unsigned)block);
        uint32_t *vals = malloc((size_t)block * 4);
        for (size_t wi = 0; wi < NWIDTHS; wi++) {
            unsigned w = width_menu[wi];
            uint64_t limit = (w == 32) ? 0xFFFFFFFFu : (((uint64_t)1 << w) - 1);
            int bl = packed_len(block, w);
            uint8_t *packed = malloc((size_t)nblocks * (size_t)bl + 8);
            uint32_t *src = malloc((size_t)block * 4);
            for (int b = 0; b < nblocks; b++) {
                for (int i = 0; i < block; i++)
                    src[i] = (uint32_t)(rng_next() % (limit + 1));
                pack(packed + (size_t)b * (size_t)bl, src, block, w);
            }
            free(src);
            uint32_t sink = 0;
            /* one warm pass, then repeat until the budget elapses */
            for (int b = 0; b < nblocks; b++)
                unpack(packed + (size_t)b * (size_t)bl, vals, block, w);
            double start = now_sec();
            int passes = 0;
            while (now_sec() - start < budget) {
                for (int b = 0; b < nblocks; b++) {
                    unpack(packed + (size_t)b * (size_t)bl, vals, block, w);
                    sink += vals[block - 1];
                }
                passes++;
            }
            double dur = now_sec() - start;
            if (sink == 1)
                fputc(' ', stderr); /* keep the loop live */
            double n = (double)passes * nblocks * block;
            double in_bytes = (double)passes * nblocks * bl;
            printf("%s\tcref\t%d\tw%u\t%.0f\t%.1f\t%.3f\t%.3f\n", label, block, w, n,
                   n / dur / 1e6, in_bytes / dur / 1e9, n * 4 / dur / 1e9);
            free(packed);
        }
        free(vals);
    }
    return 0;
}

/*
 * SuiteSparse:GraphBLAS bare-metal C baseline for the comparison
 * benchmark suite (task #180).
 *
 * Mirrors bench/comparison/lagraph_baseline.py but uses the raw C
 * GraphBLAS API directly to expose the absolute lower bound that
 * FFI-bound runtimes (Python, Go, Java) ultimately compete against.
 * The graph topology must match the NetworkX baseline (n = 16 384,
 * m = 65 536, weights in [1, 100], random.Random(31) sequence).
 * Because C does not expose Python's `random` PRNG, the C harness
 * accepts a CSV edge list on stdin so the same edges produced by
 * the Python script can be replayed verbatim. Use the helper:
 *
 *   python3 - <<'PY' > /tmp/edges.csv
 *   import random
 *   N=1<<14; E=4*N
 *   r=random.Random(31)
 *   for _ in range(E):
 *       print(f"{r.randrange(N)},{r.randrange(N)},{r.randrange(100)+1}")
 *   PY
 *
 *   ./lagraph_baseline < /tmp/edges.csv
 *
 * Build (macOS / Homebrew):
 *
 *   brew install suite-sparse                # provides libgraphblas
 *   # LAGraph isn't in Homebrew; build it locally:
 *   git clone https://github.com/GraphBLAS/LAGraph.git
 *   cd LAGraph && cmake -S . -B build -DCMAKE_BUILD_TYPE=Release \
 *       -DCMAKE_INSTALL_PREFIX=/opt/lagraph
 *   cmake --build build --target install -j
 *
 *   clang -O3 -std=c11 -I$(brew --prefix suite-sparse)/include \
 *         -I/opt/lagraph/include \
 *         -L$(brew --prefix suite-sparse)/lib \
 *         -L/opt/lagraph/lib \
 *         lagraph_baseline.c \
 *         -lgraphblas -llagraph -llagraphx -lm \
 *         -o lagraph_baseline
 *
 * Build (Linux):
 *
 *   apt-get install libsuitesparse-dev       # provides libgraphblas
 *   (build LAGraph as above)
 *   gcc -O3 -std=c11 lagraph_baseline.c -lgraphblas -llagraph -llagraphx -lm \
 *       -o lagraph_baseline
 *
 * Run:
 *
 *   ./lagraph_baseline < /tmp/edges.csv
 *
 * Output mirrors the Python harness:
 *
 *   BFS                            best of 3: <ms>
 *   Dijkstra single-source         best of 3: <ms>
 *   PageRank                       best of 3: <ms>
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include <GraphBLAS.h>
#include <LAGraph.h>
#include <LAGraphX.h>

#define N_NODES (1u << 14)
#define REPEATS 3

#define OK(expr) do { \
    GrB_Info _info = (expr); \
    if (_info != GrB_SUCCESS && _info != GrB_NO_VALUE) { \
        fprintf(stderr, "GraphBLAS error %d at %s:%d\n", _info, __FILE__, __LINE__); \
        exit(1); \
    } \
} while (0)

static double monotonic_ms(struct timespec start) {
    struct timespec now;
    clock_gettime(CLOCK_MONOTONIC, &now);
    return (now.tv_sec - start.tv_sec) * 1000.0
         + (now.tv_nsec - start.tv_nsec) / 1e6;
}

static GrB_Matrix read_edge_list_from_stdin(void) {
    /* Two-pass read: collect into dynamic arrays then GrB_Matrix_build. */
    size_t cap = 1 << 16, n = 0;
    GrB_Index *I = malloc(cap * sizeof(*I));
    GrB_Index *J = malloc(cap * sizeof(*J));
    double    *X = malloc(cap * sizeof(*X));
    if (!I || !J || !X) {
        fprintf(stderr, "out of memory\n");
        exit(1);
    }
    char line[64];
    while (fgets(line, sizeof line, stdin)) {
        if (n == cap) {
            cap *= 2;
            I = realloc(I, cap * sizeof(*I));
            J = realloc(J, cap * sizeof(*J));
            X = realloc(X, cap * sizeof(*X));
        }
        unsigned long u, v, w;
        if (sscanf(line, "%lu,%lu,%lu", &u, &v, &w) != 3) continue;
        I[n] = u;
        J[n] = v;
        X[n] = (double) w;
        ++n;
    }

    GrB_Matrix A;
    OK(GrB_Matrix_new(&A, GrB_FP64, N_NODES, N_NODES));
    OK(GrB_Matrix_build_FP64(A, I, J, X, n, GrB_FIRST_FP64));
    free(I); free(J); free(X);
    return A;
}

static double time_bfs(GrB_Matrix A) {
    double best = 1e18;
    for (int r = 0; r < REPEATS; ++r) {
        struct timespec t0; clock_gettime(CLOCK_MONOTONIC, &t0);
        GrB_Vector level = NULL, parent = NULL;
        OK(LAGr_BreadthFirstSearch_vanilla(&level, &parent, NULL, 0, false, NULL));
        (void) A;
        double dt = monotonic_ms(t0);
        if (dt < best) best = dt;
        GrB_Vector_free(&level);
        GrB_Vector_free(&parent);
    }
    return best;
}

static double time_sssp(GrB_Matrix A) {
    double best = 1e18;
    for (int r = 0; r < REPEATS; ++r) {
        struct timespec t0; clock_gettime(CLOCK_MONOTONIC, &t0);
        GrB_Vector path = NULL;
        OK(LAGr_SingleSourceShortestPath(&path, NULL, 0, 0.0, NULL));
        (void) A;
        double dt = monotonic_ms(t0);
        if (dt < best) best = dt;
        GrB_Vector_free(&path);
    }
    return best;
}

static double time_pagerank(GrB_Matrix A) {
    double best = 1e18;
    for (int r = 0; r < REPEATS; ++r) {
        struct timespec t0; clock_gettime(CLOCK_MONOTONIC, &t0);
        GrB_Vector pr = NULL;
        int iters = 0;
        OK(LAGr_PageRank(&pr, &iters, NULL, 0.85f, 1e-6f, 30, NULL));
        (void) A;
        double dt = monotonic_ms(t0);
        if (dt < best) best = dt;
        GrB_Vector_free(&pr);
    }
    return best;
}

int main(void) {
    LAGraph_Init(NULL);

    GrB_Matrix A = read_edge_list_from_stdin();
    GrB_Index n, m;
    GrB_Matrix_nrows(&n, A);
    GrB_Matrix_nvals(&m, A);
    fprintf(stderr, "SuiteSparse:GraphBLAS C | n=%llu m=%llu\n",
            (unsigned long long) n, (unsigned long long) m);

    double bfs = time_bfs(A);
    double sssp = time_sssp(A);
    double pr = time_pagerank(A);

    printf("BFS                            best of %d: %.3f ms\n", REPEATS, bfs);
    printf("Dijkstra single-source         best of %d: %.3f ms\n", REPEATS, sssp);
    printf("PageRank                       best of %d: %.3f ms\n", REPEATS, pr);

    GrB_Matrix_free(&A);
    LAGraph_Finalize(NULL);
    return 0;
}

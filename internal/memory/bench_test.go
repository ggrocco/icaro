package memory

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"icaro/internal/store"
)

// QueryTargetP95 is the retrieval latency target over a 10k-document
// project on SQLite (ADR 0020).
const QueryTargetP95 = 50 * time.Millisecond

var benchVocab = func() []string {
	r := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test corpus
	letters := "abcdefghijklmnopqrstuvwxyz"
	words := make([]string, 3000)
	for i := range words {
		n := 4 + r.Intn(6)
		var sb strings.Builder
		for j := 0; j < n; j++ {
			sb.WriteByte(letters[r.Intn(len(letters))])
		}
		words[i] = sb.String()
	}
	return words
}()

// zipf picks words with a skewed distribution, like real text.
func zipfWords(r *rand.Rand, z *rand.Zipf, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = benchVocab[z.Uint64()]
	}
	return strings.Join(parts, " ")
}

func seed10k(tb testing.TB) (*Engine, Scope) {
	tb.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.SQLite, filepath.Join(tb.TempDir(), "bench.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		tb.Fatal(err)
	}
	// Seeding speed only: a larger page cache keeps the bulk load from
	// spilling to the WAL mid-transaction. Queries run with defaults.
	if _, err := db.DB().ExecContext(ctx, `PRAGMA cache_size = -262144`); err != nil {
		tb.Fatal(err)
	}
	e := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), "bench")
	scope := Scope{ProjectID: "pbench", Origin: OriginAPI}
	r := rand.New(rand.NewSource(42))                       //nolint:gosec // deterministic test corpus
	z := rand.NewZipf(r, 1.1, 1, uint64(len(benchVocab)-1)) //nolint:gosec // constant positive length
	kinds := []string{KindDecision, KindTask, KindFinding, KindFile, KindSymbol}
	now := time.Now().UTC()
	var batch []store.MemoryBundle
	for i := 0; i < 10000; i++ {
		row := &store.MemoryRow{
			ID: store.NewID(), ProjectID: scope.ProjectID, Origin: OriginAPI, Kind: kinds[i%len(kinds)],
			Title: zipfWords(r, z, 6), Body: zipfWords(r, z, 60), Source: fmt.Sprintf("pkg%d/file%d.go", i%50, i),
			CreatedAt: now, UpdatedAt: now,
		}
		batch = append(batch, bundle(row))
		if len(batch) == 2500 {
			if err := db.PutMemories(ctx, batch); err != nil {
				tb.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	if _, err := db.DB().ExecContext(ctx, `PRAGMA cache_size = -2000`); err != nil {
		tb.Fatal(err)
	}
	return e, scope
}

func benchQueries() []Query {
	r := rand.New(rand.NewSource(7))                        //nolint:gosec // deterministic test corpus
	z := rand.NewZipf(r, 1.1, 1, uint64(len(benchVocab)-1)) //nolint:gosec // constant positive length
	qs := make([]Query, 200)
	for i := range qs {
		qs[i] = Query{Text: zipfWords(r, z, 1+i%3), Limit: 10}
		if i%4 == 0 {
			qs[i].Kinds = []string{KindDecision, KindFinding}
		}
	}
	return qs
}

func TestQueryLatency10k(t *testing.T) {
	if testing.Short() {
		t.Skip("benchmark-backed test")
	}
	if raceEnabled {
		// Latency under the race detector (which slows pure-Go SQLite by
		// an order of magnitude) says nothing about the target; CI runs
		// this test without -race.
		t.Skip("latency target is measured without -race")
	}
	start := time.Now()
	e, scope := seed10k(t)
	t.Logf("seeded 10k memories in %s", time.Since(start).Round(time.Millisecond))
	ctx := context.Background()
	var lat []time.Duration
	for _, q := range benchQueries() {
		start = time.Now()
		if _, err := e.Query(ctx, scope, q); err != nil {
			t.Fatal(err)
		}
		lat = append(lat, time.Since(start))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p50, p95 := lat[len(lat)/2], lat[len(lat)*95/100]
	target := QueryTargetP95
	t.Logf("10k docs: p50=%s p95=%s (target %s)", p50, p95, target)
	if p95 > target {
		t.Fatalf("p95 %s exceeds target %s", p95, target)
	}
}

func BenchmarkQuery10k(b *testing.B) {
	e, scope := seed10k(b)
	ctx := context.Background()
	qs := benchQueries()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Query(ctx, scope, qs[i%len(qs)]); err != nil {
			b.Fatal(err)
		}
	}
}

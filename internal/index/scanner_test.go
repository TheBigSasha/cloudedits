package index

import (
	"context"
	"testing"
)

func BenchmarkRescanUnchanged2000(b *testing.B) {
	idx, _ := openScanned(b, 2000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.ScanAll(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScanCold2000(b *testing.B) {
	ctx := context.Background()
	idx, _ := openScanned(b, 2000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if _, err := idx.db.ExecContext(ctx, `DELETE FROM assets`); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := idx.ScanAll(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

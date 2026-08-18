package monitors

import (
	"testing"
	"time"
)

// BAYATLIK (8.6) — saf fonksiyon, gerçek zamana ihtiyaç yok.
//
// 3.3'ün durum makinesi testleriyle aynı biçim: "şimdi" argüman olduğu için
// "üç aralık geçti" senaryosu beklemeden kurulabiliyor.

func TestIsStale(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	const interval = int32(60) // eşik: 180 saniye

	for name, tc := range map[string]struct {
		isActive      bool
		lastCheckedAt *time.Time
		want          bool
	}{
		"az önce kontrol edildi": {
			isActive:      true,
			lastCheckedAt: ptr(now.Add(-30 * time.Second)),
			want:          false,
		},
		"bir aralık gecikti — henüz bayat değil": {
			isActive:      true,
			lastCheckedAt: ptr(now.Add(-90 * time.Second)),
			want:          false,
		},
		"eşiğe TAM ulaştı — henüz bayat değil": {
			// Sınır DIŞLAYICI (`>`): tam üç aralık, hâlâ "gecikmiş" sayılıyor.
			// 3.3'ün eşik kararıyla aynı yön — sınırda alarm üretmemek.
			isActive:      true,
			lastCheckedAt: ptr(now.Add(-180 * time.Second)),
			want:          false,
		},
		"eşiği aştı — BAYAT": {
			isActive:      true,
			lastCheckedAt: ptr(now.Add(-181 * time.Second)),
			want:          true,
		},
		"saatlerdir ölçülmüyor — BAYAT": {
			isActive:      true,
			lastCheckedAt: ptr(now.Add(-6 * time.Hour)),
			want:          true,
		},
		"DURAKLATILMIŞ monitor bayat değil": {
			// Kullanıcı ölçmeyi bilerek durdurdu; kendi kararı arıza diye
			// ilan edilmemeli.
			isActive:      false,
			lastCheckedAt: ptr(now.Add(-6 * time.Hour)),
			want:          false,
		},
		"HİÇ kontrol edilmemiş monitor bayat değil": {
			// "Hiç ölçülmedi" ile "ölçüm durdu" farklı şeyler.
			isActive:      true,
			lastCheckedAt: nil,
			want:          false,
		},
		"duraklatılmış VE hiç kontrol edilmemiş": {
			isActive:      false,
			lastCheckedAt: nil,
			want:          false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := IsStale(tc.isActive, tc.lastCheckedAt, interval, now); got != tc.want {
				t.Errorf("IsStale = %v, beklenen %v", got, tc.want)
			}
		})
	}
}

// EŞİK MONITOR'ÜN KENDİ ARALIĞINDAN TÜRÜYOR: tek bir sabit eşik, seyrek
// kontrol edilen her monitor'ü sürekli bayat gösterirdi.
func TestStaleThresholdScalesWithInterval(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	fiveMinutesAgo := ptr(now.Add(-5 * time.Minute))

	// 30 saniyelik aralık → eşik 90 saniye → 5 dakika BAYAT.
	if !IsStale(true, fiveMinutesAgo, 30, now) {
		t.Error("30 sn aralıklı monitor 5 dakika sonra bayat sayılmadı")
	}

	// 1 saatlik aralık → eşik 3 saat → 5 dakika bayat DEĞİL.
	if IsStale(true, fiveMinutesAgo, 3600, now) {
		t.Error("1 saat aralıklı monitor 5 dakika sonra bayat sayıldı")
	}
}

// Çarpan 3.1'in kalp atışı toleransıyla aynı olmalı: eşiğin kaynağı
// projenin kendi içinde, uydurulmuş bir sayı değil.
func TestStaleAfterUsesHeartbeatMultiplier(t *testing.T) {
	t.Parallel()

	if got, want := StaleAfter(60), 180*time.Second; got != want {
		t.Errorf("StaleAfter(60) = %s, beklenen %s", got, want)
	}
	if StaleIntervalMultiplier != 3 {
		t.Errorf("çarpan = %d, beklenen 3 (3.1'in kalp atışı toleransı)", StaleIntervalMultiplier)
	}
}

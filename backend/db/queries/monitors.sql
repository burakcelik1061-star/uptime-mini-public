-- HER SORGU org_id İLE SINIRLI (tek satırlık okuma dahil).
--
-- Üyelik zaten middleware'de doğrulanıyor (1.4), yani bu koşul teoride
-- fazladan. Yine de duruyor: "id'yi bilen görür" bir sorgu, ileride yanlış
-- yere bağlanan tek bir handler'da başka organizasyonun verisini döndürürdü.
-- İzolasyonun SQL'de de yazılı olması, kontrolü tek bir katmana emanet
-- etmemek demek (bkz. docs/1.4-org-scoped-authorization.md).

-- name: CreateMonitor :one
-- `next_check_at` UYGULAMADAN GELMİYOR, `now()` ile SQL'de kuruluyor: zamanın
-- tek kaynağı veritabanı olsun. api ve worker ayrı süreçler (ve ileride ayrı
-- makineler); aralarındaki saat kayması "sırası gelmedi" diye hiç kontrol
-- edilmeyen ya da sürekli kontrol edilen monitor demek olurdu.
--
-- Aktif monitor HEMEN sıraya giriyor (now()): kullanıcı monitor'ü ekledikten
-- sonra ilk sonucu bir kontrol aralığı beklemek istemiyor. Pasif eklenen
-- monitor'ün sırası NULL — şemadaki tutarlılık kısıtı bunu şart koşuyor.
INSERT INTO monitors (
    org_id, name, url, method,
    check_interval_seconds, timeout_ms,
    expected_status_min, expected_status_max,
    degraded_response_ms,
    failure_threshold, recovery_threshold,
    is_active, current_status,
    next_check_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
        CASE WHEN $12 THEN now() ELSE NULL END)
RETURNING *;

-- name: ListMonitorsForOrg :many
-- Sıralama created_at: kullanıcının eklediği sıra tahmin edilebilir kalsın
-- (ada göre sıralamak, adı değişen bir monitor'ü listede zıplatırdı).
--
-- `server_now` DA DÖNÜYOR (8.6): panonun bayatlık kararı, `last_checked_at`i
-- yazan worker ile AYNI saate (veritabanınınkine) bakmak zorunda. api
-- sürecinin `time.Now()`u kullanılsaydı iki konteyner arasındaki kayma
-- bayatlığa dönüşürdü — 3.2'nin "zamanın kaynağı veritabanı" kararı ve
-- 7.5'in public cevapta düzelttiği aynı hata.
--
-- Ayrı bir `SELECT now()` sorgusu YAPILMIYOR: ikinci bir gidiş-geliş, hem
-- gereksiz hem de iki sorgu arasında geçen sürenin kendisi bir sapma kaynağı.
SELECT sqlc.embed(monitors), now()::timestamptz AS server_now
FROM monitors
WHERE org_id = $1
ORDER BY created_at, id;

-- name: GetMonitorForOrg :one
SELECT * FROM monitors
WHERE id = $1 AND org_id = $2;

-- name: UpdateMonitor :one
-- Kısmi güncelleme (PATCH) COALESCE ile SQL'de değil, uygulamada çözülüyor:
-- handler önce mevcut satırı okuyup gelen alanları üzerine bindiriyor, sonra
-- BİRLEŞMİŞ hâli oluşturmayla AYNI doğrulamadan geçiriyor. Aksi hâlde
-- alanlar arası kurallar (timeout <= aralık, min <= max) yalnızca şemadaki
-- CHECK'e kalırdı ve kullanıcı alan hatası yerine 500 görürdü.
--
-- ZAMANLAMA SÜTUNU DA BURADA YÖNETİLİYOR (`next_check_at`), çünkü aktiflik bu
-- uçtan değişiyor:
--   * duraklatıldı        → sıra NULL (şemadaki kısıt bunu şart koşuyor)
--   * yeniden başlatıldı  → now(), yani hemen (duraklatma boyunca dünya
--                           değişmiş olabilir; ilk sonucu beklemeye gerek yok)
--   * aktiflik değişmedi  → SIRAYA DOKUNULMUYOR. Kullanıcının adı/eşiği
--                           değiştirmesi, sırada bekleyen kontrolü öne
--                           çekmemeli; her PATCH sırayı sıfırlasaydı formu
--                           kurcalayan biri monitor'ü hiç kontrol
--                           edilmeyecek hâle getirebilirdi.
-- Kontrol aralığı değiştiğinde de sıra ellenmiyor: YENİ aralık, sıradaki
-- kontrol sahiplenildiğinde uygulanıyor (ClaimDueMonitors).
UPDATE monitors SET
    name                   = $3,
    url                    = $4,
    method                 = $5,
    check_interval_seconds = $6,
    timeout_ms             = $7,
    expected_status_min    = $8,
    expected_status_max    = $9,
    degraded_response_ms   = $10,
    failure_threshold      = $11,
    recovery_threshold     = $12,
    is_active              = $13,
    current_status         = $14,
    next_check_at          = CASE
                                 WHEN NOT $13              THEN NULL
                                 WHEN monitors.is_active   THEN monitors.next_check_at
                                 ELSE now()
                             END,
    -- Aktiflik değiştiyse durum makinesinin HAFIZASI sıfırlanıyor (3.3):
    -- duraklatmadan önce biriken iki ardışık hata, günler sonra devam
    -- ettirilen monitor'ü üçüncü hatada DOWN yapmamalı.
    consecutive_failures   = CASE WHEN $13 = monitors.is_active
                                  THEN monitors.consecutive_failures ELSE 0 END,
    consecutive_successes  = CASE WHEN $13 = monitors.is_active
                                  THEN monitors.consecutive_successes ELSE 0 END,
    status_changed_at      = CASE WHEN monitors.current_status IS DISTINCT FROM $14
                                  THEN now() ELSE monitors.status_changed_at END,
    updated_at             = now()
WHERE id = $1 AND org_id = $2
RETURNING *;

-- name: DeleteMonitor :execrows
-- Etkilenen satır sayısı dönüyor: 0 ise "yok veya başka organizasyonun" —
-- ikisi arasındaki fark üretilmiyor, ikisi de 404.
DELETE FROM monitors
WHERE id = $1 AND org_id = $2;

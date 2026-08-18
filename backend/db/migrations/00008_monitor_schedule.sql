-- +goose Up
-- Scheduler'ın saati (Faz 3.2).
--
-- 2.1'in kuralı gereği bu iki sütun burada geliyor: YAZMA YOLUNA sahip olan
-- adım bu. `next_check_at` "sıradaki kontrol ne zaman" — 0.1 §5'in kararı
-- gereği zamanlama "her monitor için bir cron" değil, tek bir sorgu:
-- `WHERE is_active AND next_check_at <= now()` + `FOR UPDATE SKIP LOCKED`.
ALTER TABLE monitors
    ADD COLUMN next_check_at   timestamptz,
    ADD COLUMN last_checked_at timestamptz;

-- NULL'IN ANLAMI VAR: "zamanlanmamış". Duraklatılmış (is_active = false) bir
-- monitor'ün sırası yok; aktif olanın MUTLAKA var. Aşağıdaki kısıt bu ikisini
-- birbirine bağlıyor, yani "aktif ama sırası olmayan" (sessizce hiç kontrol
-- edilmeyen) bir monitor şemaya giremiyor. Bu tam olarak fark edilmesi en zor
-- hata sınıfı: monitor listede 'pending' durur, kimse kontrol etmez.
UPDATE monitors SET next_check_at = now() WHERE is_active;

ALTER TABLE monitors
    ADD CONSTRAINT monitors_schedule_consistency CHECK (
        (is_active AND next_check_at IS NOT NULL)
        OR (NOT is_active AND next_check_at IS NULL)
    );

-- Claim sorgusunun indeksi ve KISMİ olması bilinçli: koşulun `is_active`
-- kısmı indeksin içinde, yani indeks yalnızca zamanlanmış satırları taşıyor.
-- Duraklatılmış monitor'ler (bir hesapta zamanla birikiyorlar) indekste hiç
-- yer tutmuyor ve "sırası gelen"i bulmak indeksin BAŞINI okumak oluyor.
CREATE INDEX monitors_due_idx ON monitors (next_check_at)
    WHERE is_active AND next_check_at IS NOT NULL;

-- last_checked_at İÇİN İNDEKS YOK: hiçbir sorgu ona göre filtrelemiyor/
-- sıralamıyor (satırla birlikte okunuyor). Kullanılmayan indeks her yazmada
-- maliyet demek — bu tablo scheduler yüzünden kontrol başına bir kez
-- güncelleniyor.

-- +goose Down
DROP INDEX IF EXISTS monitors_due_idx;
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_schedule_consistency;
ALTER TABLE monitors
    DROP COLUMN IF EXISTS last_checked_at,
    DROP COLUMN IF EXISTS next_check_at;

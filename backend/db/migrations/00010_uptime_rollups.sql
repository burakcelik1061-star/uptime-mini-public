-- +goose Up
-- Toplulaştırma tabloları (Faz 3.4) — 0.1 §6'nın "tasarımın kendisi" dediği yer.
--
-- Neden: 90 günlük status page grafiği (5.2) monitor başına ~130 bin ham satır
-- yerine 90 satır okuyacak. Bu bir optimizasyon değil; ham veri 30 gün sonra
-- BUDANIYOR (0.1 §6.3), yani geçmiş yalnızca burada yaşıyor.
--
-- İKİ TABLO, İKİ SAKLAMA SÜRESİ: saatlik 13 ay, günlük süresiz. Tek tabloda
-- `granularity` sütunuyla tutulsaydı iki farklı budama kuralı aynı tablonun
-- içinde satır silmek zorunda kalırdı.

-- Ortak sütunlar (0.1'in alan sözlüğü):
--   total_checks       — YALNIZCA ZAMANLANMIŞ kontroller. Manuel kontroller
--                        (2.3'ün `triggered_by`'ı) uptime'a girmiyor: düğmeye
--                        üst üste basmak oranı bozmamalı.
--   maintenance_checks — 4.3'e kadar her zaman 0; sütun şimdi geliyor çünkü
--                        0.1'in uptime formülünün paydası ona bağlı
--                        ((success + degraded) / (total - maintenance)) ve
--                        formülün tamamı bu tablodan okunabilmeli.
--   avg/p95            — NULL olabilir: ölçümü olan kontrol yoksa "0 ms" demek
--                        yanlış olurdu (2.4'ün aynı kararı).
--   downtime_seconds   — YAKLAŞIK: başarısız kontrol sayısı × kontrol aralığı.
--                        Kesintinin gerçek başlangıç/bitişi 4.1'in incident
--                        kayıtlarında olacak; buradaki değer "o saatte ne kadar
--                        süre başarısız ölçüm alındı" sorusunun cevabı.

CREATE TABLE uptime_rollup_hourly (
    monitor_id   uuid NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    -- Kova BAŞLANGICI (UTC, tam saat). Yarı açık aralık [start, start+1h).
    bucket_start timestamptz NOT NULL,

    total_checks       integer NOT NULL CHECK (total_checks >= 0),
    success_checks     integer NOT NULL CHECK (success_checks >= 0),
    degraded_checks    integer NOT NULL CHECK (degraded_checks >= 0),
    failure_checks     integer NOT NULL CHECK (failure_checks >= 0),
    maintenance_checks integer NOT NULL DEFAULT 0 CHECK (maintenance_checks >= 0),

    avg_response_ms integer CHECK (avg_response_ms >= 0),
    p95_response_ms integer CHECK (p95_response_ms >= 0),

    downtime_seconds integer NOT NULL DEFAULT 0 CHECK (downtime_seconds >= 0),

    computed_at timestamptz NOT NULL DEFAULT now(),

    -- PK DOĞRUDAN DOĞAL ANAHTAR (0.1'in ER şemasındaki `id bigint` yerine):
    -- bu satırlara hiçbir tablo referans vermiyor, yani yapay anahtar yalnızca
    -- fazladan bir sequence ve fazladan bir unique indeks olurdu. Asıl mesele
    -- 0.1'in şart koştuğu benzersizlik — İDEMPOTENT UPSERT'ün dayanağı — ve o
    -- burada PK'nın kendisi.
    PRIMARY KEY (monitor_id, bucket_start),

    -- Sayılar birbiriyle tutarlı olmalı: parçaların toplamı bütünü aşamaz.
    -- Toplulaştırma sorgusundaki bir hata sessizce yanlış uptime üretmesin.
    CONSTRAINT uptime_rollup_hourly_counts CHECK (
        success_checks + degraded_checks + failure_checks <= total_checks
    )
);

-- Budama sorgusunun indeksi (`WHERE bucket_start < ...`): PK monitor_id ile
-- başladığı için yalnızca zamana göre silme onu kullanamaz.
CREATE INDEX uptime_rollup_hourly_bucket_idx ON uptime_rollup_hourly (bucket_start);

CREATE TABLE uptime_rollup_daily (
    monitor_id  uuid NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    -- Gün (UTC). `date` tipi: günlük kova bir zaman damgası değil bir takvim
    -- günü; saat bilgisi taşımak "bu gün hangi saatte başlıyor" sorusunu
    -- gereksizce açardı.
    bucket_date date NOT NULL,

    total_checks       integer NOT NULL CHECK (total_checks >= 0),
    success_checks     integer NOT NULL CHECK (success_checks >= 0),
    degraded_checks    integer NOT NULL CHECK (degraded_checks >= 0),
    failure_checks     integer NOT NULL CHECK (failure_checks >= 0),
    maintenance_checks integer NOT NULL DEFAULT 0 CHECK (maintenance_checks >= 0),

    avg_response_ms integer CHECK (avg_response_ms >= 0),
    p95_response_ms integer CHECK (p95_response_ms >= 0),

    downtime_seconds integer NOT NULL DEFAULT 0 CHECK (downtime_seconds >= 0),

    computed_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (monitor_id, bucket_date),

    CONSTRAINT uptime_rollup_daily_counts CHECK (
        success_checks + degraded_checks + failure_checks <= total_checks
    )
);

-- 5.2'nin sorgusu: "şu monitor'ün son 90 günü" — PK zaten (monitor_id, date)
-- ile başlıyor, yani ek indeks GEREKMİYOR. Günlük tablo budanmadığı için
-- zamana göre ayrı bir indekse de ihtiyaç yok.

-- Toplulaştırmanın SU SEVİYESİ: hangi ana kadar işlendiği.
--
-- Neden ayrı bir tablo (özet tablolarındaki `max(bucket)` yerine): su seviyesi
-- VERİDEN türetilemez. Hiç kontrol üretmemiş bir saat özet satırı da
-- üretmiyor, yani `max(bucket)` seyrek veride İLERLEMİYOR — job her turda aynı
-- pencereyi yeniden işler ve geriye dönük doldurma sonsuza kadar takılırdı
-- (testi var). Ayrıca budama kapısının sorusu tam olarak bu: "toplulaştırma
-- nereye kadar geldi?" — "hangi kovada veri var?" değil.
CREATE TABLE rollup_state (
    -- 'hourly' | 'daily'. Serbest metin: yeni bir granülerlik eklemek
    -- migration gerektirmesin.
    name              text PRIMARY KEY,
    -- Bu ANA KADAR (hariç) olan her şey toplulaştırıldı.
    processed_through timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementBegin
-- Ham veriyi budayan fonksiyon: partition'ları DÜŞÜRÜR, satır silmez.
--
-- 2.3'ün günlük partition kararının karşılığı: `DELETE FROM check_results
-- WHERE checked_at < ...` milyonlarca satırı tek tek silip tabloyu şişirirdi
-- (ve autovacuum'a iş bırakırdı). `DROP TABLE` anlık ve bloat üretmiyor.
--
-- Hangi partition'ın düşürüleceği ADINDAN DEĞİL, katalogdaki gerçek
-- SINIRINDAN okunuyor: ad bir konvansiyon, sınır ise Postgres'in kendi
-- doğrusu. Yalnızca ÜST SINIRI p_before'dan küçük veya eşit olanlar
-- düşürülüyor — yani içinde p_before'dan yeni tek bir satır bile olabilecek
-- bir partition'a dokunulmuyor.
CREATE FUNCTION drop_check_result_partitions_before(p_before timestamptz)
RETURNS integer
LANGUAGE plpgsql
AS $$
DECLARE
    v_part    record;
    v_dropped integer := 0;
BEGIN
    FOR v_part IN
        SELECT c.relname AS name,
               (regexp_match(
                   pg_get_expr(c.relpartbound, c.oid),
                   'TO \(''([^'']+)''\)'
               ))[1] AS upper_bound
        FROM pg_class c
        JOIN pg_inherits i ON i.inhrelid = c.oid
        JOIN pg_class parent ON parent.oid = i.inhparent
        WHERE parent.relname = 'check_results'
    LOOP
        CONTINUE WHEN v_part.upper_bound IS NULL;

        IF v_part.upper_bound::timestamptz <= p_before THEN
            EXECUTE format('DROP TABLE %I', v_part.name);
            v_dropped := v_dropped + 1;
        END IF;
    END LOOP;

    RETURN v_dropped;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS drop_check_result_partitions_before(timestamptz);
DROP TABLE IF EXISTS rollup_state;
DROP TABLE IF EXISTS uptime_rollup_daily;
DROP TABLE IF EXISTS uptime_rollup_hourly;

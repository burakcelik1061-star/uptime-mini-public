-- Toplulaştırma ve budama sorguları (Faz 3.4).
--
-- Hesap TAMAMEN SQL'DE: milyonlarca ham satırı uygulamaya taşıyıp Go'da
-- toplamak, hem belleği hem ağı boşuna harcamak olurdu (2.4'ün "uptime
-- sunucuda hesaplanır" kararının devamı). Bu sorgular org-scoped DEĞİL —
-- çağıran worker, tüm kiracıların verisini toplulaştırıyor (bkz. schedule.sql).

-- name: UpsertHourlyRollups :execrows
-- Bir zaman aralığındaki ham sonuçlardan SAATLİK özetleri üretir [from, to).
--
-- İDEMPOTENT: `ON CONFLICT … DO UPDATE` ile aynı kova ikinci kez işlendiğinde
-- satır ikiye katlanmıyor, YENİDEN HESAPLANIP üzerine yazılıyor. Bu bir konfor
-- değil zorunluluk (0.1 §6.2): worker yeniden başladığında, bir tur atlandığında
-- ve geriye dönük doldurmada (backfill) aynı kod çalışıyor. Ayrıca DEVAM EDEN
-- saat de her turda yeniden hesaplanıyor — kova dolmadan da grafik veri
-- gösterebilsin, ve saat kapandığında değer kendiliğinden düzelsin.
--
-- YALNIZCA `scheduled`: manuel kontroller (2.3) uptime'a girmiyor.
--
-- `date_trunc(… , checked_at)` UTC'de: sunucu oturumunun saat dilimi
-- kovaların sınırını kaydırmasın.
--
-- BAKIM PENCERESİ (4.3): pencereye düşen kontroller `maintenance_checks`
-- olarak AYRICA sayılıyor ve success/degraded/failure sayılarına GİRMİYOR.
-- İkisine birden girselerdi 0.1'in formülü ((success + degraded) /
-- (total - maintenance)) %100'ü AŞABİLİRDİ: 10 kontrolün 2'si bakımda ve
-- başarılıysa pay 10, payda 8 olurdu. Yani `total` üç değil DÖRT parçadan
-- oluşuyor; şemadaki "parçaların toplamı bütünü aşamaz" kısıtı da bu ayrımla
-- korunuyor. Yanıt süresi istatistikleri de bakım kontrollerini dışarıda
-- bırakıyor: bakım sırasındaki yavaşlık servisin performansı hakkında bir şey
-- söylemiyor.
INSERT INTO uptime_rollup_hourly (
    monitor_id, bucket_start,
    total_checks, success_checks, degraded_checks, failure_checks,
    maintenance_checks,
    avg_response_ms, p95_response_ms, downtime_seconds, computed_at
)
SELECT
    c.monitor_id,
    date_trunc('hour', c.checked_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
    count(*),
    count(*) FILTER (WHERE NOT mw.in_maintenance AND c.outcome = 'success'),
    count(*) FILTER (WHERE NOT mw.in_maintenance AND c.outcome = 'degraded'),
    count(*) FILTER (WHERE NOT mw.in_maintenance AND c.outcome = 'failure'),
    count(*) FILTER (WHERE mw.in_maintenance),
    -- Ortalama `avg()` ile DEĞİL sum/count ile: `avg()` boş kümede NULL döner
    -- ve sqlc cast'in NULL olabileceğini göremez (2.4'te yaşandı). Bölme
    -- yalnızca örnek varsa yapılıyor.
    CASE
        WHEN count(c.response_time_ms) FILTER (WHERE NOT mw.in_maintenance) > 0
        THEN (sum(c.response_time_ms) FILTER (WHERE NOT mw.in_maintenance)
              / count(c.response_time_ms) FILTER (WHERE NOT mw.in_maintenance))::integer
    END,
    -- p95: `percentile_disc` GERÇEK bir ölçümü döndürüyor (interpolasyon
    -- yapan `percentile_cont` yerine) — "95. persentil 143.7 ms" diye bir
    -- ölçüm alınmadı, alınan bir ölçüm gösteriliyor.
    percentile_disc(0.95) WITHIN GROUP (
        ORDER BY CASE WHEN NOT mw.in_maintenance THEN c.response_time_ms END
    )::integer,
    -- Kesinti süresi YAKLAŞIK: başarısız kontrol × kontrol aralığı. Her
    -- başarısız ölçüm, bir sonraki kontrole kadar geçen süreyi temsil ediyor.
    -- Gerçek başlangıç/bitiş 4.1'in incident kayıtlarında olacak.
    (count(*) FILTER (WHERE NOT mw.in_maintenance AND c.outcome = 'failure')
        * max(m.check_interval_seconds))::integer,
    now()
FROM check_results c
JOIN monitors m ON m.id = c.monitor_id
-- "Bu kontrol bir bakım penceresine düşüyor mu": satır başına EXISTS.
-- LATERAL, monitor bazlı indeks (maintenance_window_monitors_monitor_idx)
-- üzerinden tek bir arama; hiç penceresi olmayan organizasyonlarda ilk
-- adımda boş dönüyor.
CROSS JOIN LATERAL (
    SELECT EXISTS (
        SELECT 1
        FROM maintenance_window_monitors mwm
        JOIN maintenance_windows w ON w.id = mwm.window_id
        WHERE mwm.monitor_id = c.monitor_id
          AND c.checked_at >= w.starts_at
          AND c.checked_at <  w.ends_at
    ) AS in_maintenance
) mw
WHERE c.triggered_by = 'scheduled'
  AND c.checked_at >= sqlc.arg(from_time)
  AND c.checked_at <  sqlc.arg(to_time)
GROUP BY c.monitor_id, 2
ON CONFLICT (monitor_id, bucket_start) DO UPDATE SET
    total_checks       = EXCLUDED.total_checks,
    success_checks     = EXCLUDED.success_checks,
    degraded_checks    = EXCLUDED.degraded_checks,
    failure_checks     = EXCLUDED.failure_checks,
    maintenance_checks = EXCLUDED.maintenance_checks,
    avg_response_ms    = EXCLUDED.avg_response_ms,
    p95_response_ms    = EXCLUDED.p95_response_ms,
    downtime_seconds   = EXCLUDED.downtime_seconds,
    computed_at        = EXCLUDED.computed_at;

-- name: UpsertDailyRollups :execrows
-- GÜNLÜK özetler — HAM VERİDEN, saatlik özetlerden DEĞİL.
--
-- 0.1 §6.2 günlük özetin saatliklerden türetilmesini önermiş ama p95 için
-- "3.4'te tekrar değerlendirilecek" demişti. Değerlendirme sonucu: ham veriden.
-- Sebep — saatlik p95'lerin ortalaması GÜNLÜK p95 DEĞİLDİR (persentil
-- toplanabilir bir istatistik değil) ve süresiz saklanan tablo tam olarak
-- yanlış olmaması gereken tablo. Ham veri her zaman elimizde: budama yalnızca
-- TOPLULAŞTIRMASI YAPILMIŞ aralığı siliyor (bkz. retention job'ı), yani günlük
-- kova hesaplanmadan o günün ham satırları düşürülmüyor.
INSERT INTO uptime_rollup_daily (
    monitor_id, bucket_date,
    total_checks, success_checks, degraded_checks, failure_checks,
    maintenance_checks,
    avg_response_ms, p95_response_ms, downtime_seconds, computed_at
)
SELECT
    c.monitor_id,
    (c.checked_at AT TIME ZONE 'UTC')::date,
    count(*),
    count(*) FILTER (WHERE NOT mw.in_maintenance AND c.outcome = 'success'),
    count(*) FILTER (WHERE NOT mw.in_maintenance AND c.outcome = 'degraded'),
    count(*) FILTER (WHERE NOT mw.in_maintenance AND c.outcome = 'failure'),
    count(*) FILTER (WHERE mw.in_maintenance),
    CASE
        WHEN count(c.response_time_ms) FILTER (WHERE NOT mw.in_maintenance) > 0
        THEN (sum(c.response_time_ms) FILTER (WHERE NOT mw.in_maintenance)
              / count(c.response_time_ms) FILTER (WHERE NOT mw.in_maintenance))::integer
    END,
    percentile_disc(0.95) WITHIN GROUP (
        ORDER BY CASE WHEN NOT mw.in_maintenance THEN c.response_time_ms END
    )::integer,
    (count(*) FILTER (WHERE NOT mw.in_maintenance AND c.outcome = 'failure')
        * max(m.check_interval_seconds))::integer,
    now()
FROM check_results c
JOIN monitors m ON m.id = c.monitor_id
-- "Bu kontrol bir bakım penceresine düşüyor mu": satır başına EXISTS.
-- LATERAL, monitor bazlı indeks (maintenance_window_monitors_monitor_idx)
-- üzerinden tek bir arama; hiç penceresi olmayan organizasyonlarda ilk
-- adımda boş dönüyor.
CROSS JOIN LATERAL (
    SELECT EXISTS (
        SELECT 1
        FROM maintenance_window_monitors mwm
        JOIN maintenance_windows w ON w.id = mwm.window_id
        WHERE mwm.monitor_id = c.monitor_id
          AND c.checked_at >= w.starts_at
          AND c.checked_at <  w.ends_at
    ) AS in_maintenance
) mw
WHERE c.triggered_by = 'scheduled'
  AND c.checked_at >= sqlc.arg(from_time)
  AND c.checked_at <  sqlc.arg(to_time)
GROUP BY c.monitor_id, 2
ON CONFLICT (monitor_id, bucket_date) DO UPDATE SET
    total_checks       = EXCLUDED.total_checks,
    success_checks     = EXCLUDED.success_checks,
    degraded_checks    = EXCLUDED.degraded_checks,
    failure_checks     = EXCLUDED.failure_checks,
    maintenance_checks = EXCLUDED.maintenance_checks,
    avg_response_ms    = EXCLUDED.avg_response_ms,
    p95_response_ms    = EXCLUDED.p95_response_ms,
    downtime_seconds   = EXCLUDED.downtime_seconds,
    computed_at        = EXCLUDED.computed_at;

-- name: GetRollupState :one
-- Toplulaştırmanın su seviyesi: bu ana kadar (hariç) her şey işlendi.
--
-- Job penceresini yalnızca "son N saat" olarak seçseydik, worker bir hafta
-- kapalı kaldığında o haftanın kovaları SONSUZA KADAR boş kalırdı. Su
-- seviyesinden devam etmek geriye dönük doldurmayı (backfill) kendiliğinden
-- veriyor: aynı job, aynı kod.
--
-- Hiç kayıt yoksa `pgx.ErrNoRows` — Go tarafında "ilk kez çalışıyor"
-- durumundan ayırt edilebilir bir sinyal.
SELECT processed_through FROM rollup_state WHERE name = sqlc.arg(name);

-- name: SetRollupState :exec
-- Su seviyesini ilerletir. GERİYE ALMIYOR (`GREATEST`): eş zamanlı iki worker
-- kopyası farklı pencereler işlerse, yavaş olanın eski değeri hızlının
-- ilerlemesini geri sarmamalı.
INSERT INTO rollup_state (name, processed_through, updated_at)
VALUES (sqlc.arg(name), sqlc.arg(processed_through), now())
ON CONFLICT (name) DO UPDATE SET
    processed_through = GREATEST(rollup_state.processed_through, EXCLUDED.processed_through),
    updated_at        = now();

-- name: OldestScheduledCheck :one
-- Ham veride duran en eski ZAMANLANMIŞ kontrol. İlk kez çalışan (veya
-- rollup tabloları boş olan) bir sistemde toplulaştırmanın nereden
-- başlayacağını söylüyor.
-- Yalnızca rollup tabloları BOŞKEN çağrılıyor (ilk kurulum / temiz veritabanı),
-- yani partition'lar üzerindeki bu tarama sıcak yolda değil.
SELECT checked_at FROM check_results WHERE triggered_by = 'scheduled'
ORDER BY checked_at LIMIT 1;

-- name: DeleteHourlyRollupsBefore :execrows
-- Saatlik özetlerin saklama süresi 13 ay (0.1 §6.3): yıl-üstü karşılaştırma
-- yapılabilsin ama tablo sonsuza kadar büyümesin. GÜNLÜK özetler hiç
-- silinmiyor — yılda ~36 bin satır, budamanın maliyeti faydasından büyük.
DELETE FROM uptime_rollup_hourly WHERE bucket_start < sqlc.arg(before_time);

-- name: EnsureCheckResultPartitions :one
-- 2.3'ün migration'daki fonksiyonunu ileri kaydırır: pencere hep bugün + N gün.
-- Çağrılmazsa partition'lar biter ve DEFAULT partition olmadığı için (2.3'ün
-- bilinçli kararı) ilk kontrol yazılamaz — yüksek sesle hata verir.
SELECT ensure_check_result_partitions(sqlc.arg(days_ahead)::integer);

-- name: DropCheckResultPartitionsBefore :one
-- Ham veriyi budar: üst sınırı verilen andan eski partition'ları DÜŞÜRÜR.
SELECT drop_check_result_partitions_before(sqlc.arg(before_time)::timestamptz);

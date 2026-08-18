-- Zaman serisi sorguları (Faz 2.3).
--
-- Okuma sorguları `monitors` ile JOIN'li ve org_id şart koşuyor: monitor'ün
-- hangi organizasyona ait olduğu tek doğruluk kaynağı olarak orada duruyor,
-- check_results satırında org_id KOPYALANMIYOR. Kopyalansaydı monitor başka
-- bir organizasyona taşındığında (bugün böyle bir akış yok ama) geçmiş veri
-- yanlış yerde görünürdü. Bedeli PK üzerinden tek bir ek arama.

-- name: InsertCheckResult :one
INSERT INTO check_results (
    monitor_id, checked_at, outcome, triggered_by,
    http_status, response_time_ms, error_type, error_message
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListRecentChecks :many
-- "Bir monitor'ün son N kontrolü" — detay sayfasının (2.4) sıcak sorgusu.
-- (monitor_id, checked_at DESC) indeksiyle sıralama işi zaten yapılmış
-- durumda; `id DESC` yalnızca aynı milisaniyeye düşen satırların sırasını
-- belirlemek için (deterministik sayfalama).
SELECT c.*
FROM check_results c
JOIN monitors m ON m.id = c.monitor_id
WHERE c.monitor_id = $1 AND m.org_id = $2
ORDER BY c.checked_at DESC, c.id DESC
LIMIT sqlc.arg(row_limit);

-- name: ListChecksInRange :many
-- "Şu aralıktaki kontroller" — yanıt süresi grafiği (2.4) ve toplulaştırma
-- (3.4) bu deseni kullanıyor.
--
-- checked_at KOŞULU PARTITION PRUNING'İN DAYANAĞI: aralık verildiğinde
-- Postgres yalnızca ilgili günlerin partition'larını okuyor, 30 günün
-- tamamını değil.
SELECT c.*
FROM check_results c
JOIN monitors m ON m.id = c.monitor_id
WHERE c.monitor_id = sqlc.arg(monitor_id)
  AND m.org_id = sqlc.arg(org_id)
  AND c.checked_at >= sqlc.arg(from_time)
  AND c.checked_at < sqlc.arg(to_time)
ORDER BY c.checked_at DESC, c.id DESC
LIMIT sqlc.arg(row_limit);

-- name: SummarizeChecksInRange :one
-- Bir aralıktaki ham uptime özeti (2.4).
--
-- MANUEL KONTROLLER UPTIME'A GİRMİYOR (2.3'ün `triggered_by` kararının
-- karşılığı): kullanıcının "şimdi kontrol et" düğmesine üst üste basması
-- oranı çarpıtmamalı. Sayıları ayrıca dönüyoruz ki arayüz "bu aralıkta N
-- manuel kontrol var" diyebilsin.
--
-- Hesap SQL'de yapılıyor, satırlar uygulamaya taşınmıyor: liste ucu 200
-- satırla sınırlı ve o listeden hesaplanan bir yüzde 30 günlük aralıkta
-- SESSİZCE yanlış olurdu.
-- BAKIM PENCERESİ (4.3): pencereye düşen kontroller ayrı sayılıyor ve
-- success/degraded/failure sayılarına GİRMİYOR — 3.4'ün özet tablolarıyla
-- birebir aynı bölüşüm. İkisine birden girselerdi 0.1'in formülü
-- ((success + degraded) / (total - maintenance)) %100'ü aşabilirdi.
SELECT
    count(*) FILTER (WHERE c.triggered_by = 'scheduled')                             AS scheduled_checks,
    count(*) FILTER (WHERE c.triggered_by = 'scheduled' AND NOT mw.in_maintenance
                       AND c.outcome = 'success')                                    AS success_checks,
    count(*) FILTER (WHERE c.triggered_by = 'scheduled' AND NOT mw.in_maintenance
                       AND c.outcome = 'degraded')                                   AS degraded_checks,
    count(*) FILTER (WHERE c.triggered_by = 'scheduled' AND NOT mw.in_maintenance
                       AND c.outcome = 'failure')                                    AS failure_checks,
    count(*) FILTER (WHERE c.triggered_by = 'scheduled' AND mw.in_maintenance)       AS maintenance_checks,
    count(*) FILTER (WHERE c.triggered_by = 'manual')                                AS manual_checks,
    -- ORTALAMA DEĞİL, TOPLAM + ÖRNEK SAYISI dönüyor. İki sebep:
    --   * `avg()` hiç satır yoksa NULL döner ve sqlc bir cast'in NULL
    --     olabileceğini göremiyor — çalışma anında "NULL'ı float64'e
    --     tarayamadım" hatası olurdu;
    --   * ortalamayı Go'da hesaplamak "örnek yok" ile "ortalama 0 ms"yi
    --     ayırt edilebilir kılıyor (0 ms artık gerçek bir ölçüm, bkz. 2.3).
    -- Ölçümü olmayan satırlar (cevap alınamayan kontroller) count() ve sum()
    -- tarafından zaten atlanıyor.
    coalesce(sum(c.response_time_ms) FILTER (
        WHERE c.triggered_by = 'scheduled' AND NOT mw.in_maintenance), 0)::bigint    AS response_time_total_ms,
    count(c.response_time_ms) FILTER (
        WHERE c.triggered_by = 'scheduled' AND NOT mw.in_maintenance)                AS response_time_samples
FROM check_results c
JOIN monitors m ON m.id = c.monitor_id
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
WHERE c.monitor_id = sqlc.arg(monitor_id)
  AND m.org_id = sqlc.arg(org_id)
  AND c.checked_at >= sqlc.arg(from_time)
  AND c.checked_at < sqlc.arg(to_time);

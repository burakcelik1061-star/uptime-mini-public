-- Bildirim kanalları ve gönderim defteri (Faz 6.3).

-- --- 1. Kanallar (org-scoped) ------------------------------------------------

-- name: ListNotificationChannels :many
-- Organizasyonun kanalları.
SELECT * FROM notification_channels
WHERE org_id = sqlc.arg(org_id)
ORDER BY created_at, id;

-- name: CreateNotificationChannel :one
INSERT INTO notification_channels (org_id, kind, name, target)
VALUES (sqlc.arg(org_id), sqlc.arg(kind), sqlc.arg(name), sqlc.arg(target))
RETURNING *;

-- name: GetNotificationChannel :one
-- `org_id` KOŞULU ŞART (1.4'ün disiplini): kimlik yoldan geliyor, sahiplik
-- sorgunun içinde doğrulanıyor. Yabancı bir kanal kimliği satırsız dönüyor.
SELECT * FROM notification_channels
WHERE id = sqlc.arg(id) AND org_id = sqlc.arg(org_id);

-- name: DeleteNotificationChannel :execrows
DELETE FROM notification_channels
WHERE id = sqlc.arg(id) AND org_id = sqlc.arg(org_id);

-- --- 2. Gönderim defteri (worker) --------------------------------------------

-- name: EnqueueIncidentNotifications :execrows
-- Bildirilmesi gereken (kanal × incident × olay) üçlülerini deftere yazar.
--
-- KUYRUĞA YAZMA INCIDENT YAZMA YOLUNDA DEĞİL, TARAMA İLE: 4.1'in
-- `ApplyTransition`ına bir "bildirim satırları da üret" adımı eklenseydi,
-- kesinti kaydı yazmak kanal listesini okumaya ve fazladan bir yazmaya bağımlı
-- hâle gelirdi — yani bildirim altyapısı kesinti defterini bloke edebilirdi.
-- Tarama bunu tersine çeviriyor: incident yazma yolu 6.3'ten HABERSİZ.
--
-- Yan faydası: worker kapalıyken açılan bir kesinti de, worker geri gelince
-- bildiriliyor.
--
-- "İKİ KEZ GÖNDERME" garantisi `ON CONFLICT DO NOTHING` + şemadaki UNIQUE
-- kısıtı: aynı üçlü ikinci kez yazılamıyor, iki worker kopyası çalışsa bile.
--
-- İKİ SÜZGEÇ ZAMANLA İLGİLİ VE İKİSİ DE BİLİNÇLİ:
--   (a) olay, kanal OLUŞTURULDUKTAN sonra gerçekleşmiş olmalı — yeni eklenen
--       bir kanal geçmişin tamamını posta kutusuna boşaltmasın;
--   (b) olay `lookback` penceresinin içinde olmalı — worker bir hafta kapalı
--       kaldıysa açılışta bir bildirim çığı üretmesin.
INSERT INTO notification_deliveries (channel_id, incident_id, event)
SELECT c.id, i.id, e.event
FROM notification_channels c
JOIN incidents i ON i.org_id = c.org_id
CROSS JOIN LATERAL (
    VALUES
        ('incident_opened'::notification_event,   i.started_at),
        ('incident_resolved'::notification_event, i.resolved_at)
) AS e(event, occurred_at)
WHERE c.is_active
  AND e.occurred_at IS NOT NULL
  AND e.occurred_at >= c.created_at
  AND e.occurred_at > now() - sqlc.arg(lookback)::interval
ON CONFLICT (channel_id, incident_id, event) DO NOTHING;

-- name: ClaimPendingDeliveries :many
-- Sırası gelmiş gönderimleri ATOMİK olarak sahiplenir (3.2'nin deseni).
--
-- `FOR UPDATE SKIP LOCKED` + aynı ifadede `next_attempt_at`i ileri almak:
-- iki worker kopyası aynı satırı ele geçiremiyor ve kilit bırakıldığında satır
-- artık "sırası gelmiş" değil. Gönderim dış dünyaya istek atan, saniyeler
-- sürebilen bir iş — "önce SELECT et, sonra UPDATE et" burada gerçekten iki
-- bildirim demekti.
--
-- Kanal ve incident bilgisi aynı sorguda dönüyor: gönderimi hazırlamak için
-- ikinci bir tur gerekmesin.
WITH due AS (
    SELECT d.id
    FROM notification_deliveries d
    WHERE d.status = 'pending' AND d.next_attempt_at <= now()
    ORDER BY d.next_attempt_at
    LIMIT sqlc.arg(row_limit)
    FOR UPDATE SKIP LOCKED
)
UPDATE notification_deliveries d
SET next_attempt_at = now() + sqlc.arg(claim_lease)::interval
FROM due
WHERE d.id = due.id
RETURNING d.id, d.channel_id, d.incident_id, d.event, d.attempts,
    (SELECT c.kind   FROM notification_channels c WHERE c.id = d.channel_id) AS channel_kind,
    (SELECT c.name   FROM notification_channels c WHERE c.id = d.channel_id) AS channel_name,
    (SELECT c.target FROM notification_channels c WHERE c.id = d.channel_id) AS channel_target,
    (SELECT o.name FROM organizations o
      JOIN notification_channels c ON c.org_id = o.id WHERE c.id = d.channel_id) AS org_name,
    (SELECT i.title       FROM incidents i WHERE i.id = d.incident_id) AS incident_title,
    (SELECT i.impact      FROM incidents i WHERE i.id = d.incident_id) AS incident_impact,
    (SELECT i.started_at  FROM incidents i WHERE i.id = d.incident_id) AS incident_started_at,
    (SELECT i.resolved_at FROM incidents i WHERE i.id = d.incident_id) AS incident_resolved_at;

-- name: MarkDeliverySent :exec
UPDATE notification_deliveries
SET status = 'sent', attempts = attempts + 1, sent_at = now(), last_error = NULL
WHERE id = sqlc.arg(id);

-- name: MarkDeliveryFailed :exec
-- Başarısız gönderim: deneme sayısı üst sınıra ulaştıysa `failed`, aksi hâlde
-- geri çekilerek yeniden denenmek üzere `pending`.
--
-- SINIRSIZ DENEME YOK: ulaşılamayan bir hedef (silinmiş webhook, kapanmış
-- posta kutusu) sonsuza kadar tur başına bir hata satırı üretirdi.
UPDATE notification_deliveries
SET attempts = attempts + 1,
    last_error = sqlc.arg(last_error),
    status = CASE
        WHEN attempts + 1 >= sqlc.arg(max_attempts)::int THEN 'failed'::notification_status
        ELSE 'pending'::notification_status
    END,
    next_attempt_at = now() + (sqlc.arg(backoff)::interval * (attempts + 1))
WHERE id = sqlc.arg(id);

-- name: CountDeliveriesByStatus :many
-- Kanal ekranındaki özet: kanal başına gönderilmiş/bekleyen/başarısız sayısı.
SELECT channel_id, status, count(*) AS total
FROM notification_deliveries
WHERE channel_id = ANY(sqlc.arg(channel_ids)::uuid[])
GROUP BY channel_id, status;

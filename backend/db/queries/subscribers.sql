-- Status page aboneleri (Faz 6.4).
--
-- Bu dosyadaki sorgular OTURUMSUZ bir yüzeyi besliyor (5.1'in public ağacı):
-- kimlik ya yoldaki SLUG ya da linkteki TOKEN. Org izolasyonu bu yüzden
-- middleware'e değil sorguların içine yazılı.

-- name: UpsertSubscriber :one
-- Aboneliği yazar; adres zaten varsa YENİ BİR ONAY LİNKİ üretir.
--
-- ENUMERATION SAVUNMASININ TEMELİ BU İFADE: "zaten kayıtlı" durumu ayrı bir
-- yol üretmiyor, aynı ifade her iki durumda da tek satır dönüyor. Çağıran
-- taraf da her iki durumda aynı cevabı veriyor — aksi hâlde bir yabancı,
-- denediği adreslerden hangilerinin o sayfaya abone olduğunu öğrenirdi.
--
-- ONAYLANMIŞ ABONE YENİDEN ONAY İSTEMİYOR: `status` ve `confirmed_at`
-- korunuyor, yalnızca yeni bir onay token'ı yazılıyor (kullanılmasa da
-- zararsız). Aksi hâlde bir yabancı, başkasının onaylı aboneliğini
-- "beklemede"ye düşürebilirdi.
INSERT INTO status_page_subscribers (
    status_page_id, email, confirm_token_hash, confirm_sent_at, unsubscribe_token
) VALUES (
    sqlc.arg(status_page_id), sqlc.arg(email),
    sqlc.arg(confirm_token_hash), now(), sqlc.arg(unsubscribe_token)
)
ON CONFLICT (status_page_id, email) DO UPDATE
SET confirm_token_hash = EXCLUDED.confirm_token_hash,
    confirm_sent_at    = now(),
    updated_at         = now()
RETURNING *;

-- name: ConfirmSubscriber :one
-- Onay token'ıyla aboneliği onaylar ve token'ı YAKAR.
--
-- Token tek kullanımlık: `confirm_token_hash = NULL` yazılıyor, yani aynı link
-- ikinci kez çalışmıyor. `confirmed_at` yalnızca ilk onayda yazılıyor
-- (COALESCE) — aksi hâlde "ne zamandır abone" sorusu her tıklamada
-- sıfırlanırdı.
UPDATE status_page_subscribers
SET status             = 'confirmed',
    confirmed_at       = COALESCE(confirmed_at, now()),
    confirm_token_hash = NULL,
    updated_at         = now()
WHERE confirm_token_hash = sqlc.arg(confirm_token_hash)
RETURNING *;

-- name: GetSubscriberByUnsubscribeToken :one
SELECT s.*, sp.slug AS page_slug
FROM status_page_subscribers s
JOIN status_pages sp ON sp.id = s.status_page_id
WHERE s.unsubscribe_token = sqlc.arg(unsubscribe_token);

-- name: DeleteSubscriber :execrows
-- Abonelikten çıkma: satır SİLİNİYOR, bayrak konmuyor.
--
-- "Ayrıldı" bayrağı, ayrılmış bir adresi süresiz saklamak demekti; kişinin
-- isteği tam olarak bunun tersi. Gönderim defteri satırları da cascade
-- düşüyor.
DELETE FROM status_page_subscribers WHERE id = sqlc.arg(id);

-- name: CountConfirmedSubscribers :one
-- Ekrandaki sayaç (admin tarafı).
SELECT count(*) FROM status_page_subscribers
WHERE status_page_id = sqlc.arg(status_page_id) AND status = 'confirmed';

-- --- Bildirim kuyruğu ---------------------------------------------------------

-- name: ListSubscribableIncidentEvents :many
-- Aboneleri olan sayfalara ait, penceredeki kesinti olayları.
--
-- GÖRÜNÜRLÜK BURADA DEĞİL: 5.1'in "hangi kesinti ziyaretçiye görünür" kuralı
-- bu sorguya YENİDEN YAZILMIYOR. Sorgu adayları döndürüyor, kararı çağıran
-- taraf 5.1'in KENDİ sorgularına (internal/statuspage) sorarak veriyor —
-- 6.2'de public kanal kapısı için verilen kararın aynısı.
-- İki olay UNION ALL ile: `LATERAL (VALUES …)` biçimi aynı işi görüyor ama
-- sqlc'nin çözümleyicisi bu konumda takma ad sütunlarını göremiyor. Açık
-- yazım hem üretilebiliyor hem okurken daha az iş çıkarıyor.
SELECT i.id AS incident_id, i.org_id, sp.id AS status_page_id,
       'incident_opened'::notification_event AS event,
       i.started_at AS occurred_at,
       i.title, i.impact, i.started_at, i.resolved_at
FROM incidents i
JOIN status_pages sp ON sp.org_id = i.org_id AND sp.is_published
WHERE i.started_at > now() - sqlc.arg(lookback)::interval
  AND EXISTS (
      SELECT 1 FROM status_page_subscribers s
      WHERE s.status_page_id = sp.id AND s.status = 'confirmed'
  )

UNION ALL

SELECT i.id, i.org_id, sp.id,
       'incident_resolved'::notification_event,
       i.resolved_at,
       i.title, i.impact, i.started_at, i.resolved_at
FROM incidents i
JOIN status_pages sp ON sp.org_id = i.org_id AND sp.is_published
WHERE i.resolved_at IS NOT NULL
  AND i.resolved_at > now() - sqlc.arg(lookback)::interval
  AND EXISTS (
      SELECT 1 FROM status_page_subscribers s
      WHERE s.status_page_id = sp.id AND s.status = 'confirmed'
  )

-- Sıralama YOK: kuyruğa yazma idempotent (ON CONFLICT DO NOTHING), yani
-- adayların hangi sırayla işlendiği sonucu değiştirmiyor.
LIMIT sqlc.arg(row_limit);

-- name: ListIncidentMonitorIDs :many
-- Kesintinin kapsadığı servisler — GÖRÜNÜRLÜK KAPISINA verilecek liste.
SELECT monitor_id FROM incident_monitors WHERE incident_id = sqlc.arg(incident_id);

-- name: EnqueueSubscriberNotifications :execrows
-- Onaylanmış abonelere gönderim satırı yazar.
--
-- İki süzgeç 6.3'ün kararlarının aynısı: abone olay ANINDAN ÖNCE onaylanmış
-- olmalı (yeni abone geçmişi posta kutusunda bulmasın) ve tekrar yazma
-- `ON CONFLICT DO NOTHING` ile engellenmiş (garanti şemadaki UNIQUE'te).
INSERT INTO subscriber_deliveries (subscriber_id, incident_id, event)
SELECT s.id, sqlc.arg(incident_id), sqlc.arg(event)
FROM status_page_subscribers s
WHERE s.status_page_id = sqlc.arg(status_page_id)
  AND s.status = 'confirmed'
  AND s.confirmed_at <= sqlc.arg(occurred_at)
ON CONFLICT (subscriber_id, incident_id, event) DO NOTHING;

-- name: ClaimPendingSubscriberDeliveries :many
-- Sırası gelmiş abone gönderimlerini ATOMİK sahiplenir (3.2'nin deseni).
WITH due AS (
    SELECT d.id
    FROM subscriber_deliveries d
    WHERE d.status = 'pending' AND d.next_attempt_at <= now()
    ORDER BY d.next_attempt_at
    LIMIT sqlc.arg(row_limit)
    FOR UPDATE SKIP LOCKED
)
UPDATE subscriber_deliveries d
SET next_attempt_at = now() + sqlc.arg(claim_lease)::interval
FROM due
WHERE d.id = due.id
RETURNING d.id, d.subscriber_id, d.incident_id, d.event, d.attempts,
    (SELECT s.email FROM status_page_subscribers s WHERE s.id = d.subscriber_id) AS email,
    (SELECT s.unsubscribe_token FROM status_page_subscribers s
      WHERE s.id = d.subscriber_id) AS unsubscribe_token,
    (SELECT sp.slug FROM status_pages sp
      JOIN status_page_subscribers s ON s.status_page_id = sp.id
     WHERE s.id = d.subscriber_id) AS page_slug,
    (SELECT COALESCE(sp.title, o.name) FROM status_pages sp
      JOIN organizations o ON o.id = sp.org_id
      JOIN status_page_subscribers s ON s.status_page_id = sp.id
     WHERE s.id = d.subscriber_id) AS page_title,
    (SELECT i.title       FROM incidents i WHERE i.id = d.incident_id) AS incident_title,
    (SELECT i.impact      FROM incidents i WHERE i.id = d.incident_id) AS incident_impact,
    (SELECT i.started_at  FROM incidents i WHERE i.id = d.incident_id) AS incident_started_at,
    (SELECT i.resolved_at FROM incidents i WHERE i.id = d.incident_id) AS incident_resolved_at;

-- name: MarkSubscriberDeliverySent :exec
UPDATE subscriber_deliveries
SET status = 'sent', attempts = attempts + 1, sent_at = now(), last_error = NULL
WHERE id = sqlc.arg(id);

-- name: MarkSubscriberDeliveryFailed :exec
UPDATE subscriber_deliveries
SET attempts = attempts + 1,
    last_error = sqlc.arg(last_error),
    status = CASE
        WHEN attempts + 1 >= sqlc.arg(max_attempts)::int THEN 'failed'::notification_status
        ELSE 'pending'::notification_status
    END,
    next_attempt_at = now() + (sqlc.arg(backoff)::interval * (attempts + 1))
WHERE id = sqlc.arg(id);

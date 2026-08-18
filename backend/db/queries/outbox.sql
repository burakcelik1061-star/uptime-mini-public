-- Geliştirici posta kutusu (Faz 8.2).
--
-- İKİ SORGU VE İKİSİ DE ORG-SCOPED DEĞİL: bu tablo bir ürün yüzeyi değil,
-- mock e-posta adapter'ının posta kutusu (bkz. 00018'in yorumu). Erişimi
-- sınırlayan şey bir `WHERE org_id` değil, ucun yalnızca APP_ENV açıkça
-- development|test iken router'a kaydedilmesi.

-- name: RecordOutboxMessage :exec
-- Mock adapter'ın gönderdiği mesajı saklar.
--
-- `:exec` — dönen satıra ihtiyaç yok. Çağıran, yazma başarısız olsa bile
-- gönderimi BAŞARILI sayıyor (adapters.go'da gerekçesi yazılı): posta
-- kutusu bir geliştirme aracı ve onun arızası abonelik akışını düşürmemeli.
INSERT INTO email_outbox (recipient, subject, body)
VALUES (sqlc.arg(recipient), sqlc.arg(subject), sqlc.arg(body));

-- name: ListOutboxMessages :many
-- En yeni mesajlar.
--
-- SINIR ZORUNLU ARGÜMAN, varsayılanı olan bir sayı değil: sınırsız bir liste,
-- aylardır açık duran bir geliştirme ortamında ekranı da cevabı da şişirirdi
-- (00018'de budama job'ı bilinçli olarak yok).
SELECT * FROM email_outbox
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit);

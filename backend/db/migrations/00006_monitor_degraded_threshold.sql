-- +goose Up
-- "Çalışıyor ama YAVAŞ" eşiği (Faz 2.2).
--
-- 2.1'in kuralı gereği bu sütun burada geliyor: yazma yoluna sahip olan adım
-- bu — kontrol sonucunun `degraded` olup olmadığı KONTROL ANINDA karara
-- bağlanıyor (0.1 §5). Eşiği sonradan uygulayıp geçmişi yeniden yorumlamak
-- mümkün değil: eşik değiştiğinde eski kontroller anlamını değiştirirdi.
--
-- NULL = yavaşlık sinyali KAPALI. Varsayılan bilerek NULL: "makul" bir
-- gecikme eşiği hedefe göre değişiyor (bir statik sayfa ile bir rapor ucu
-- aynı değil) ve uydurulmuş bir varsayılan, kullanıcının hiç istemediği
-- 'degraded' kayıtları üretirdi.
ALTER TABLE monitors
    ADD COLUMN degraded_response_ms integer;

-- Eşik ZAMAN AŞIMINDAN büyük olamaz: daha yavaş bir cevap zaten timeout'a
-- takılır, yani ulaşılamayan bir eşik sessiz bir yapılandırma hatası olurdu.
ALTER TABLE monitors
    ADD CONSTRAINT monitors_degraded_response_ms_check
    CHECK (
        degraded_response_ms IS NULL
        OR (degraded_response_ms BETWEEN 1 AND timeout_ms)
    );

-- +goose Down
ALTER TABLE monitors DROP CONSTRAINT IF EXISTS monitors_degraded_response_ms_check;
ALTER TABLE monitors DROP COLUMN IF EXISTS degraded_response_ms;

-- +goose Up
-- PLAN & KOTA (Faz 8.5).
--
-- NEDEN VAR: org başına hiçbir kaynak limiti yoktu. Tek bir hesap istediği
-- kadar monitor açıp hepsini 30 saniyede bir kontrol ettirebiliyordu — yani
-- ürünün maliyetini belirleyen şeyin hiçbir sınırı yoktu.
--
-- 0.1 §3 "billing / plan kademeleri"ni MVP dışına almıştı ("canny-mini'de
-- yapıldı, tekrar etmenin öğrenme değeri yok") ve bu hâlâ doğru: buradaki iş
-- fatura kesmek DEĞİL, ürünün kaynak tüketimine bir kapı koymak. Ödeme
-- tarafı bilinçli olarak MOCK (bkz. internal/billing).
CREATE TYPE org_plan AS ENUM ('free', 'pro');

-- VARSAYILAN 'free': yeni açılan her organizasyon ücretsiz planda başlıyor.
-- Mevcut satırlar da (7.4'ün demo verisi dahil) bu değeri alıyor; 8.7'nin
-- seed'i Acme'yi bilinçli olarak PRO'ya çekiyor çünkü onun 60 saniyelik
-- monitor'leri FREE planın 300 saniyelik alt sınırına sığmıyor — demo
-- verisi kendi planıyla ÇELİŞMEMELİ.
ALTER TABLE organizations
    ADD COLUMN plan org_plan NOT NULL DEFAULT 'free';

-- KOTA DEĞERLERİ ŞEMADA DEĞİL KODDA (internal/plans).
--
-- Sütun olarak taşımak ("max_monitors integer") her organizasyona kendi
-- limitini yazmak demekti ve iki sorun doğururdu: (1) plan değiştiğinde
-- limitleri de UPDATE etmek gerekir, ikisi ayrışabilir; (2) bir plandaki
-- sayıyı değiştirmek migration ister. Kaynak tek olsun: satır PLANI
-- taşıyor, planın ne anlama geldiğini kod söylüyor.
--
-- Sayısal kısıt yine de ŞEMADA duruyor ve orada kalıyor: monitor tablosunun
-- `check_interval_seconds BETWEEN 30 AND 86400` kısıtı (00005) ÜRÜNÜN mutlak
-- sınırı. Planın alt sınırı onun ÜSTÜNE biniyor, yerine geçmiyor.

-- +goose Down
ALTER TABLE organizations DROP COLUMN IF EXISTS plan;
DROP TYPE IF EXISTS org_plan;

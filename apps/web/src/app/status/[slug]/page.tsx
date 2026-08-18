import type { Metadata } from 'next';
import { notFound } from 'next/navigation';

import { PublicStatusLive } from '@/components/PublicStatusLive';
import { SubscribeForm } from '@/components/SubscribeForm';
import { PublicStatusView } from '@/components/PublicStatusView';
import { fetchPublicStatus } from '@/lib/public-api';

/**
 * PUBLIC STATUS SAYFASI (Faz 5.1) — bu projenin oturum gerektirmeyen tek ekranı.
 *
 * SUNUCUDA RENDER EDİLİYOR ve bu, panonun geri kalanından bilinçli bir ayrım:
 *
 *   - 0.2 "public status page'de SSR/SEO" diyor. Bir status page'in arama
 *     sonucunda ve link önizlemesinde görünmesi ürünün kendisi; istemcide
 *     doldurulan bir sayfa ilk boyamada boş gelirdi.
 *   - Veri Next sunucusundan api'ye gidiyor (bkz. lib/public-api.ts), yani
 *     `authFetch` de CORS da bu yola hiç girmiyor.
 *   - Ziyaretçi HİÇBİR ŞEY YAZMIYOR (0.1 §2): sayfada tek bir form/düğme yok.
 *     6.2 buna TEK bir istisna getirdi: sayfa artık public kanala bağlanıp
 *     değişiklikte kendini sunucudan tazeliyor (bkz. PublicStatusLive).
 *     Ziyaretçi yine hiçbir şey YAZMIYOR; e-posta aboneliği 6.4'ün konusu.
 *
 * ROTA YALNIZCA VERİ GETİRİYOR; görünüm `PublicStatusView`de (5.3'te ayrıldı)
 * çünkü ayarlar ekranının canlı önizlemesi de aynı bileşeni çiziyor. Önizleme
 * "buna benzer bir şey" değil, sayfanın KENDİSİ olmalı.
 *
 * SIZINTI KURALI SUNUCUDA (db/queries/status_pages.sql): burada "bunu
 * göstermeli miyim" kararı verilmiyor. İkinci bir karar noktası, iki kuralın
 * ayrışabileceği ikinci bir yer demek olurdu.
 */

export async function generateMetadata({
  params,
}: PageProps<'/status/[slug]'>): Promise<Metadata> {
  const { slug } = await params;
  const page = await fetchPublicStatus(slug);

  if (!page) return { title: 'Status page bulunamadı' };

  return {
    // Başlık AYNEN kullanılıyor, sonuna "durumu" EKLENMİYOR: 5.3'ten itibaren
    // org kendi başlığını yazabiliyor ve "Acme durumu" yazan birinin sekmesinde
    // "Acme durumu durumu" görünürdü (tarayıcıda yakalandı).
    title: page.title,
    description:
      page.description ??
      `${page.title} servislerinin güncel durumu ve kesinti geçmişi.`,
  };
}

export default async function PublicStatusPageRoute({
  params,
  searchParams,
}: PageProps<'/status/[slug]'>) {
  const { slug } = await params;
  const query = await searchParams;
  const page = await fetchPublicStatus(slug);

  // Yayımlanmamış sayfa ile olmayan slug AYNI cevabı alıyor (sunucu da ayırt
  // etmiyor): fark üretmek, hangi slug'ların kayıtlı olduğunu dışarıya
  // söylemek olurdu.
  if (!page) notFound();

  return (
    <main>
      <PublicStatusView page={page} />

      {/* ABONELİK (6.4): ziyaretçinin yazdığı tek yüzey. Sunucu action'ı
          kullanıyor — istek yine Next sunucusundan api'ye gidiyor ve
          JavaScript kapalıyken de çalışıyor. */}
      <SubscribeForm
        slug={page.slug}
        result={subscribeResult(query.subscribe)}
      />
      {/* CANLI GÜNCELLEME (6.2): hiçbir şey çizmiyor, yalnızca public kanala
          bağlanıp değişiklikte sayfayı SUNUCUDAN tazeliyor. SSR bozulmuyor —
          ilk boyama yine sunucudan dolu geliyor. */}
      <PublicStatusLive slug={page.slug} />
    </main>
  );
}

/**
 * subscribeResult, adres satırındaki sonucu doğrular.
 *
 * Ham değer ekrana YAZILMIYOR: sunucu yalnızca bilinen üç sonuçtan birini
 * tanıyor, tanımadığını yok sayıyor. Aksi hâlde adres satırına yazılan bir
 * metin sayfada görünürdü.
 */
function subscribeResult(
  raw: string | string[] | undefined,
): 'ok' | 'invalid' | 'error' | undefined {
  const value = Array.isArray(raw) ? raw[0] : raw;

  return value === 'ok' || value === 'invalid' || value === 'error'
    ? value
    : undefined;
}

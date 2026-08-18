import { NextResponse, type NextRequest } from 'next/server';

import { tenantSlugFromHost } from '@/lib/tenant';

/**
 * ÇOK KİRACILI YÖNLENDİRME (Faz 5.4).
 *
 * DOSYA ADI `proxy.ts`, `middleware.ts` DEĞİL: Next 16 "middleware" dosya
 * konvansiyonunu kullanımdan kaldırdı ve aynı kancayı `proxy` adıyla sunuyor
 * (eski ad dev sunucusunda uyarı üretiyor). Mekanizma aynı: istek rotalara
 * ulaşmadan önce çalışan sunucu tarafı kanca.
 *
 * Tek iş yapıyor: `acme.localhost:3100/` isteğini sunucuda `/status/acme`
 * olarak çalıştırmak.
 *
 * KARARLAR
 *
 *   - REWRITE, redirect DEĞİL. Ziyaretçinin adres çubuğunda alt alan adı
 *     kalıyor — "kendi alan adım" hissi ürünün kendisi ve redirect her ziyarete
 *     fazladan bir tur eklerdi.
 *
 *   - PATH TABANLI ERİŞİM AYNEN ÇALIŞIYOR (`/status/acme`): ikisi de aynı
 *     rotaya düşüyor, yani ikinci bir sayfa ya da ikinci bir veri yolu yok.
 *
 *   - BİLİNMEYEN ALT ALAN ADI İÇİN BURADA ÖZEL BİR DAL YOK. `yok.localhost/`
 *     isteği `/status/yok`a yazılıyor; sayfa slug'ı bulamayınca zaten anlamlı
 *     404'ü çiziyor (`app/status/[slug]/not-found.tsx`). Burada ikinci
 *     bir "bu slug var mı" kontrolü, her istekte fazladan bir sorgu ve aynı
 *     kuralın ikinci bir kopyası demekti — üstelik yayımlanmamış sayfa ile
 *     olmayan sayfayı ayırt etmemek de o kopyada yeniden karar verilirdi.
 *
 *   - YALNIZCA KÖK YOL yazılıyor. Alt alan adı bir TAKMA AD: kiracının status
 *     sayfasının adresi. Panonun adresi ana alan adı ve alt alan adında da
 *     aynı şekilde çalışıyor — her yolu status sayfasına yazmak, `/monitors`
 *     gibi adreslerde status sayfası göstermek (ve aynı içeriği sonsuz adreste
 *     yayımlamak) olurdu.
 */
export default function proxy(request: NextRequest) {
  const slug = tenantSlugFromHost(request.headers.get('host'));

  if (slug === null || request.nextUrl.pathname !== '/') {
    return NextResponse.next();
  }

  const url = request.nextUrl.clone();
  url.pathname = `/status/${slug}`;

  return NextResponse.rewrite(url);
}

export const config = {
  /**
   * Statik dosyalar ve Next'in kendi yolları buraya hiç girmiyor:
   * `/_next/static/...` isteğini status sayfasına yazmak sayfayı stilsiz
   * bırakırdı.
   */
  matcher: ['/((?!_next/static|_next/image|favicon.ico).*)'],
};

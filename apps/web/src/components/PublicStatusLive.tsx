'use client';

import { useRouter } from 'next/navigation';
import { useEffect, useRef } from 'react';

import { API_BASE_URL } from '@/lib/api';

/**
 * Public status sayfasının canlı güncellemesi (Faz 6.2).
 *
 * SSR BOZULMUYOR — bu adımın en önemli kısıtı. Sayfa hâlâ tamamen sunucuda
 * render ediliyor (5.1/0.2'nin SEO kararı); bu bileşen HİÇBİR ŞEY ÇİZMİYOR,
 * yalnızca bir soket açıyor. Değişiklik geldiğinde yaptığı tek şey
 * `router.refresh()`:
 *
 *   - Next sunucusu server component'i yeniden çalıştırıyor, yani veri yine
 *     SSR yolundan (Next → api) geliyor. Tarayıcı api'den VERİ ÇEKMİYOR.
 *   - 5.1'in sızıntı kuralı olduğu yerde kalıyor: hangi servis/kesinti
 *     görünecek kararı yine public SORGULARDA. Canlı yayın o kararın ikinci
 *     bir kopyasını taşımıyor (olay zaten gövdesiz).
 *   - İlk boyama etkilenmiyor: JavaScript kapalı bir tarayıcıda sayfa aynen
 *     çalışmaya devam ediyor, sadece canlı güncelleme olmuyor.
 *
 * 5.1 "ziyaretçi tarayıcısından api'ye tek istek bile gitmiyor" diyordu; 6.2
 * bunu BİLİNÇLİ olarak yalnızca WebSocket için deliyor. CORS bu yola
 * uygulanmıyor (tarayıcı el sıkışmayı same-origin politikasına tabi tutmuyor)
 * ve public kanalda kimlik doğrulaması olmadığı için Origin denetimi de yok —
 * yoksa 5.4'ün alt alan adından erişimi kırılırdı.
 */

const MIN_RETRY_MS = 1_000;
const MAX_RETRY_MS = 30_000;

function retryDelay(attempt: number): number {
  const base = Math.min(MIN_RETRY_MS * 2 ** (attempt - 1), MAX_RETRY_MS);
  return Math.round(base * (0.8 + Math.random() * 0.4));
}

export function PublicStatusLive({ slug }: { slug: string }) {
  const router = useRouter();
  const routerRef = useRef(router);

  useEffect(() => {
    routerRef.current = router;
  }, [router]);

  useEffect(() => {
    let closed = false;
    let socket: WebSocket | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let attempt = 0;

    const connect = () => {
      if (closed) return;

      const base = API_BASE_URL.replace(/^http/, 'ws');
      const next = new WebSocket(
        `${base}/v1/public/ws?slug=${encodeURIComponent(slug)}`,
      );
      socket = next;

      next.onopen = () => {
        attempt = 0;
      };

      // OLAYIN TÜRÜNE BAKILMIYOR: kanaldan gelen her şey "veri değişti"
      // demek. Sunucu zaten yalnızca o sayfayı ilgilendiren, gövdesiz bir
      // sinyal gönderiyor. `realtime.ready` de tazeleme sebebi — kopukken
      // kaçırılan değişiklikler böylece yeniden bağlanmada yakalanıyor.
      next.onmessage = () => routerRef.current.refresh();

      next.onclose = () => {
        if (closed || socket !== next) return;
        attempt += 1;
        timer = setTimeout(connect, retryDelay(attempt));
      };
    };

    connect();

    return () => {
      closed = true;
      clearTimeout(timer);
      socket?.close(1000, '');
      socket = null;
    };
  }, [slug]);

  return null;
}
